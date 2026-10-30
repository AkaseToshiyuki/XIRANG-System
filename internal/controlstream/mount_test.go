package controlstream

import (
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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
