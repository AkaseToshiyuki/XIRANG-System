package mountcontrol

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func mountOperation() *xirangv1.SharedMountOperation {
	return &xirangv1.SharedMountOperation{OperationId: "operation_1", SpaceId: "share_a", Action: xirangv1.SharedMountOperation_ACTION_MOUNT, SourceHost: "storage.example", SourcePath: "/srv/share", MountPath: "/mnt/share", ExpiresAt: timestamppb.New(time.Now().Add(20 * time.Second))}
}

func TestMountProtocolAllowlist(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		action xirangv1.SharedMountOperation_Action
		args   []string
	}{
		{xirangv1.SharedMountOperation_ACTION_PREPARE, []string{"prepare", "share_a", "/srv/share"}},
		{xirangv1.SharedMountOperation_ACTION_EXPORT, []string{"export", "share_a", "/srv/share"}},
		{xirangv1.SharedMountOperation_ACTION_UNEXPORT, []string{"unexport", "share_a", "/srv/share"}},
		{xirangv1.SharedMountOperation_ACTION_MOUNT, []string{"mount", "share_a", "storage.example", "/srv/share", "/mnt/share"}},
		{xirangv1.SharedMountOperation_ACTION_UNMOUNT, []string{"unmount", "share_a", "/mnt/share"}},
	} {
		op := mountOperation()
		op.Action = tc.action
		if op.Action != xirangv1.SharedMountOperation_ACTION_MOUNT {
			op.SourceHost = ""
			if op.Action == xirangv1.SharedMountOperation_ACTION_UNMOUNT {
				op.SourcePath = ""
			} else {
				op.MountPath = ""
			}
		}
		got, err := Arguments(op, now)
		if err != nil || !reflect.DeepEqual(got, tc.args) {
			t.Fatalf("%s: %v %v", tc.action, got, err)
		}
	}
	for name, change := range map[string]func(*xirangv1.SharedMountOperation){
		"unknown": func(o *xirangv1.SharedMountOperation) { o.Action = 99 }, "traversal": func(o *xirangv1.SharedMountOperation) { o.SourcePath = "/srv/../etc" },
		"root": func(o *xirangv1.SharedMountOperation) { o.MountPath = "/" }, "shell": func(o *xirangv1.SharedMountOperation) { o.SourceHost = "server;id" },
		"option": func(o *xirangv1.SharedMountOperation) { o.SourceHost = "-o" }, "argument": func(o *xirangv1.SharedMountOperation) { o.MountPath = "/mnt/a b" },
		"space_id": func(o *xirangv1.SharedMountOperation) { o.SpaceId = "../fstab" }, "operation_id": func(o *xirangv1.SharedMountOperation) { o.OperationId = "x.service/../y" },
		"expired": func(o *xirangv1.SharedMountOperation) { o.ExpiresAt = timestamppb.New(now.Add(-time.Second)) }, "future": func(o *xirangv1.SharedMountOperation) { o.ExpiresAt = timestamppb.New(now.Add(time.Minute)) },
		"missing_time": func(o *xirangv1.SharedMountOperation) { o.ExpiresAt = nil }, "unused_fields": func(o *xirangv1.SharedMountOperation) { o.Action = xirangv1.SharedMountOperation_ACTION_EXPORT },
	} {
		t.Run(name, func(t *testing.T) {
			o := mountOperation()
			change(o)
			if _, err := Arguments(o, now); err == nil {
				t.Fatal("invalid operation accepted")
			}
		})
	}
}

type testTransport struct {
	messages  chan *xirangv1.ControlMessage
	forgotten chan string
}

func (transport *testTransport) EnqueueTransient(_ string, message *xirangv1.ControlMessage) error {
	transport.messages <- message
	return nil
}
func (transport *testTransport) ForgetTransient(_, id string) { transport.forgotten <- id }
func newTestTransport() *testTransport {
	return &testTransport{make(chan *xirangv1.ControlMessage, 8), make(chan string, 8)}
}

func TestMountBrokerNodeBindingAuditAndCompletion(t *testing.T) {
	transport := newTestTransport()
	audits := []domain.AuditEvent{}
	broker := New(transport, func(_ context.Context, event domain.AuditEvent) error { audits = append(audits, event); return nil })
	node := domain.Node{ID: "node_a", Capabilities: []string{Capability}}
	ctx := identity.WithPrincipal(context.Background(), domain.Principal{Subject: "Alice", AuthMethod: "api_token"})
	done := make(chan error, 1)
	original := mountOperation()
	unchanged := proto.Clone(original)
	go func() { done <- broker.Run(ctx, node, original) }()
	message := <-transport.messages
	if !proto.Equal(original, unchanged) {
		t.Fatal("caller operation mutated")
	}
	if err := broker.ReportMountResult(ctx, "node_b", &xirangv1.SharedMountResult{OperationId: message.MessageId, Success: true}); err == nil {
		t.Fatal("wrong node accepted")
	}
	select {
	case <-done:
		t.Fatal("wrong-node result completed operation")
	default:
	}
	if err := broker.Run(ctx, node, mountOperation()); err == nil {
		t.Fatal("overlap accepted")
	}
	if err := broker.ReportMountResult(ctx, node.ID, &xirangv1.SharedMountResult{OperationId: message.MessageId, Success: true}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if <-transport.forgotten != message.MessageId {
		t.Fatal("command not removed")
	}
	if len(audits) != 2 || audits[0].Outcome != "requested" || audits[1].Outcome != "succeeded" || audits[1].ActorID != "Alice" {
		t.Fatalf("audit: %+v", audits)
	}
	if err := broker.ReportMountResult(ctx, node.ID, &xirangv1.SharedMountResult{OperationId: message.MessageId, Success: true}); err != nil {
		t.Fatal("late duplicate rejected")
	}
}

func TestMountBrokerTimeoutAndAuditFailureDoNotDispatchAgain(t *testing.T) {
	transport := newTestTransport()
	node := domain.Node{ID: "node_a", Capabilities: []string{Capability}}
	failed := New(transport, func(context.Context, domain.AuditEvent) error { return errors.New("read-only") })
	if err := failed.Run(context.Background(), node, mountOperation()); err == nil {
		t.Fatal("audit failure allowed dispatch")
	}
	select {
	case <-transport.messages:
		t.Fatal("dispatch before audit")
	default:
	}
	<-transport.forgotten
	broker := New(transport)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := broker.Run(ctx, node, mountOperation()); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("timeout: %v", err)
	}
	message := <-transport.messages
	if <-transport.forgotten != message.MessageId {
		t.Fatal("timeout left queued command")
	}
	node.Capabilities = nil
	if err := broker.Run(context.Background(), node, mountOperation()); err == nil {
		t.Fatal("legacy node accepted")
	}
}
