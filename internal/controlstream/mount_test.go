package controlstream

import (
	"context"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type quotaCapabilityNodes struct {
	testHeartbeater
	capabilities []string
}

func (nodes *quotaCapabilityNodes) ReportCapabilities(_ context.Context, _ string, capabilities []string) error {
	nodes.capabilities = append([]string(nil), capabilities...)
	return nil
}

func TestQuotaCapabilityRequiresAgentOfferAndConfiguredMountService(t *testing.T) {
	for _, tc := range []struct {
		name       string
		offered    []string
		configured bool
		wantMount  bool
		wantQuota  bool
	}{
		{"supported", []string{mountcontrol.Capability, mountcontrol.QuotaCapability}, true, true, true},
		{"legacy-agent", []string{mountcontrol.Capability}, true, true, false},
		{"unconfigured-controller", []string{mountcontrol.Capability, mountcontrol.QuotaCapability}, false, false, false},
		{"quota-without-mount", []string{mountcontrol.QuotaCapability}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := &quotaCapabilityNodes{}
			server := New(nodes, time.Second)
			if tc.configured {
				server.SetMountReporter(mountcontrol.New(NewHub(8)))
			}
			hello := helloMessage("node_test")
			hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, tc.offered...)
			stream := newTestStream("node_test", []*xirangv1.AgentMessage{hello})
			if err := server.Connect(stream); err != nil {
				t.Fatal(err)
			}
			for _, capabilities := range [][]string{stream.sent[0].GetHelloAccepted().NegotiatedCapabilities, nodes.capabilities} {
				if hasCapability(capabilities, mountcontrol.Capability) != tc.wantMount || hasCapability(capabilities, mountcontrol.QuotaCapability) != tc.wantQuota {
					t.Fatalf("wrong acknowledged or persisted capabilities: %v", capabilities)
				}
			}
		})
	}
}

func TestQuotaOperationTraversesNegotiatedSessionAndResult(t *testing.T) {
	hub := NewHub(8)
	broker := mountcontrol.New(hub)
	nodes := &quotaCapabilityNodes{}
	server := New(nodes, time.Second, hub)
	server.SetMountReporter(broker)
	stream := newAsyncTestStream("node_test")
	ctx, cancel := context.WithTimeout(stream.ctx, 5*time.Second)
	defer cancel()
	stream.ctx = ctx
	connected := make(chan error, 1)
	go func() { connected <- server.Connect(stream) }()
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, mountcontrol.Capability, mountcontrol.QuotaCapability)
	stream.incoming <- hello
	select {
	case message := <-stream.outgoing:
		if !hasCapability(message.GetHelloAccepted().NegotiatedCapabilities, mountcontrol.QuotaCapability) {
			t.Fatal("quota offer lost during handshake")
		}
	case <-ctx.Done():
		t.Fatal("handshake timed out")
	}
	// HelloAccepted is sent before Connect registers the session with the hub.
	// Transient commands require that registration and must not queue offline.
	for !hub.NodeActive("node_test") {
		select {
		case err := <-connected:
			t.Fatalf("control stream closed before registration: %v", err)
		case <-ctx.Done():
			t.Fatal("control session registration timed out")
		case <-time.After(time.Millisecond):
		}
	}
	operation := &xirangv1.SharedMountOperation{Action: xirangv1.SharedMountOperation_ACTION_QUOTA_SET, SpaceId: "share_default", SourcePath: "/srv/share", QuotaBytes: 8 << 20}
	completed := make(chan error, 1)
	go func() {
		completed <- broker.Run(ctx, domain.Node{ID: "node_test", Capabilities: nodes.capabilities}, operation)
	}()
	select {
	case message := <-stream.outgoing:
		quota := message.GetSharedMountOperation()
		if quota == nil || quota.Action != operation.Action || quota.QuotaBytes != operation.QuotaBytes {
			t.Fatalf("quota command changed or missing: %v", message)
		}
		stream.incoming <- &xirangv1.AgentMessage{MessageId: "quota-result", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_SharedMountResult{SharedMountResult: &xirangv1.SharedMountResult{OperationId: message.MessageId, Success: true}}}
	case err := <-completed:
		t.Fatalf("quota failed before reaching the Agent: %v", err)
	case <-ctx.Done():
		t.Fatal("quota dispatch timed out")
	}
	if err := <-completed; err != nil {
		t.Fatalf("quota result not accepted: %v", err)
	}
	close(stream.incoming)
	if err := <-connected; err != nil {
		t.Fatal(err)
	}
}

func TestMountResultRequiresNegotiationAndBoundedPayload(t *testing.T) {
	for _, scenario := range []string{"valid-late-result", "unoffered", "unconfigured", "invalid-id", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			hello := helloMessage("node_test")
			if scenario != "unoffered" {
				hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, mountcontrol.Capability)
			}
			result := &xirangv1.SharedMountResult{OperationId: "mount_late", Success: true}
			if scenario == "invalid-id" {
				result.OperationId = ""
			}
			if scenario == "oversized" {
				result.Message = strings.Repeat("x", 1025)
			}
			stream := newTestStream("node_test", []*xirangv1.AgentMessage{hello, {MessageId: "result", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_SharedMountResult{SharedMountResult: result}}})
			server := New(&testHeartbeater{}, time.Second)
			if scenario != "unconfigured" {
				server.SetMountReporter(mountcontrol.New(NewHub(8)))
			}
			err := server.Connect(stream)
			if scenario == "valid-late-result" {
				if err != nil {
					t.Fatal(err)
				}
			} else if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("invalid result accepted: %v", err)
			}
			if scenario == "unconfigured" && hasCapability(stream.sent[0].GetHelloAccepted().NegotiatedCapabilities, mountcontrol.Capability) {
				t.Fatal("unconfigured capability advertised")
			}
		})
	}
}

func transientMount(id string) *xirangv1.ControlMessage {
	return &xirangv1.ControlMessage{MessageId: id, SentAt: timestamppb.Now(), Payload: &xirangv1.ControlMessage_SharedMountOperation{SharedMountOperation: &xirangv1.SharedMountOperation{OperationId: id, SpaceId: "share", Action: xirangv1.SharedMountOperation_ACTION_EXPORT, SourcePath: "/srv/share", ExpiresAt: timestamppb.New(time.Now().Add(time.Second))}}}
}

func TestMountTransientCommandsNeverReplayOnReplacementOrDisconnect(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			hub := NewHub(8)
			first := hub.Attach("node")
			if err := hub.EnqueueTransient("node", transientMount("op")); err != nil {
				t.Fatal(err)
			}
			if inFlight {
				if _, ok := first.Take(); !ok {
					t.Fatal("not queued")
				}
			}
			if !replace {
				first.Detach()
			}
			second := hub.Attach("node")
			if message, ok := second.Take(); ok {
				t.Fatalf("replayed mount command: %v", message)
			}
			if _, ok := first.Acknowledge("op"); ok {
				t.Fatal("old session acknowledged a transient command")
			}
			second.Detach()
		}
	}
}

func TestMountTransientQueueRequiresActiveAuthorizationAndCanForget(t *testing.T) {
	hub := NewHub(1)
	if err := hub.EnqueueTransient("node", transientMount("op")); err == nil {
		t.Fatal("offline request queued")
	}
	unauthorized := hub.attachAuthorized("node", func() bool { return false })
	if err := hub.EnqueueTransient("node", transientMount("op")); err == nil {
		t.Fatal("unauthorized request queued")
	}
	unauthorized.Detach()
	session := hub.Attach("node")
	if err := hub.Enqueue("node", transientMount("op")); err == nil {
		t.Fatal("durable mount queue accepted")
	}
	if err := hub.EnqueueBatch(map[string][]*xirangv1.ControlMessage{"node": {transientMount("op")}}); err == nil {
		t.Fatal("batch mount queue accepted")
	}
	if err := hub.EnqueueTransient("node", transientMount("op")); err != nil {
		t.Fatal(err)
	}
	if err := hub.EnqueueTransient("node", transientMount("op2")); err != ErrCommandQueueFull {
		t.Fatal("capacity bypass")
	}
	hub.ForgetTransient("node", "op")
	if _, ok := session.Take(); ok {
		t.Fatal("forgotten command dispatched")
	}
	if err := hub.EnqueueTransient("node", transientMount("op3")); err != nil {
		t.Fatal(err)
	}
	if _, ok := session.Take(); !ok {
		t.Fatal("new command blocked")
	}
	hub.ForgetTransient("node", "op3")
	if _, ok := session.Acknowledge("op3"); ok {
		t.Fatal("forgotten command remains in flight")
	}
}
