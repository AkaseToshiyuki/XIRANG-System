package controlstream

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type failingEventRepository struct {
	hosthealth.Repository
	fail bool
}

func (repository *failingEventRepository) PutSystemEvents(ctx context.Context, nodeID string, events []domain.SystemEvent) error {
	if repository.fail {
		return errors.New("injected persistence failure")
	}
	return repository.Repository.PutSystemEvents(ctx, nodeID, events)
}

func TestHostEventAcknowledgementRequiresNegotiationAndDurableCommit(t *testing.T) {
	for _, scenario := range []string{"committed", "write-failed", "not-negotiated", "legacy-reporter", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := boltstore.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			repository := &failingEventRepository{Repository: store, fail: scenario == "write-failed"}
			service := hosthealth.New(repository)
			hello := helloMessage("node_test")
			if scenario != "not-negotiated" {
				hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, hosthealth.OutboxCapability)
			}
			event := &xirangv1.SystemEvent{Source: "journal", SourceRecordId: "durable-cursor", Message: "corrected ECC", Kind: "corrected", Severity: "warning", OccurredAt: timestamppb.New(time.Now().Add(-48 * time.Hour))}
			if scenario == "invalid" {
				event.Kind = "repair"
			}
			message := &xirangv1.AgentMessage{MessageId: "events", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_HostEvents{HostEvents: &xirangv1.HostEventBatch{BatchId: "batch-1", Events: []*xirangv1.SystemEvent{event}}}}
			stream := newTestStream("node_test", []*xirangv1.AgentMessage{hello, message, message})
			server := New(&testHeartbeater{}, time.Second)
			server.SetHostHealthReporter(service)
			if scenario == "legacy-reporter" {
				server.SetHostHealthReporter(&healthTestReporter{})
			}
			err = server.Connect(stream)
			if scenario == "committed" {
				if err != nil || len(stream.sent) != 3 || stream.sent[1].GetHostEventsCommitted().GetBatchId() != "batch-1" || stream.sent[2].GetHostEventsCommitted().GetBatchId() != "batch-1" {
					t.Fatalf("missing commit ACK: %v %v", stream.sent, err)
				}
			} else {
				want := codes.InvalidArgument
				if scenario == "write-failed" {
					want = codes.Unavailable
				}
				if status.Code(err) != want || len(stream.sent) != 1 {
					t.Fatalf("failed batch acknowledged: %v %v", stream.sent, err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = boltstore.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			events, err := store.ListSystemEvents(context.Background(), "node_test", "", 200)
			want := 0
			if scenario == "committed" {
				want = 1
			}
			if err != nil || len(events) != want {
				t.Fatalf("durable deduplicated event count=%d want=%d err=%v", len(events), want, err)
			}
			if len(events) > 0 && (events[0].NodeID != "node_test" || events[0].Actor != "") {
				t.Fatal("authenticated node or operator boundary lost")
			}
		})
	}
}

type healthTestReporter struct {
	nodeID string
	calls  int
}

func TestLocatorTraversesRealQueueStreamAndDurableAcknowledgement(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single-on"
		if batch {
			name = "batch-off"
		}
		t.Run(name, func(t *testing.T) {
			store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			health := hosthealth.New(store)
			action := domain.BMCAction{ID: "locator-" + name, NodeID: "node_test", Action: "identify-on", Status: "queued", CreatedAt: time.Now().UTC()}
			if batch {
				action.Action = "identify-off"
			}
			if err := health.RecordAction(context.Background(), action, domain.Principal{Role: "admin", Subject: "operator"}); err != nil {
				t.Fatal(err)
			}
			command := &xirangv1.ControlMessage{MessageId: action.ID, SentAt: timestamppb.Now(), Payload: &xirangv1.ControlMessage_SetBmcIdentify{SetBmcIdentify: &xirangv1.SetBMCIdentify{Enabled: !batch, ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second))}}}
			hub := NewHub(4)
			if batch {
				err = hub.EnqueueBatch(map[string][]*xirangv1.ControlMessage{"node_test": {command}})
			} else {
				err = hub.Enqueue("node_test", command)
			}
			if err != nil {
				t.Fatalf("real queue rejected locator: %v", err)
			}
			server := New(&testHeartbeater{}, time.Second, hub)
			server.SetHostHealthReporter(health)
			stream := newAsyncTestStream("node_test")
			streamContext, cancel := context.WithTimeout(stream.ctx, 5*time.Second)
			stream.ctx = streamContext
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.Connect(stream) }()
			hello := helloMessage("node_test")
			hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, hosthealth.Capability, hosthealth.IdentifyCapability)
			stream.incoming <- hello
			if accepted := <-stream.outgoing; !hasCapability(accepted.GetHelloAccepted().NegotiatedCapabilities, hosthealth.IdentifyCapability) {
				t.Fatal("locator capability not negotiated")
			}
			select {
			case delivered := <-stream.outgoing:
				if delivered.MessageId != action.ID || delivered.GetSetBmcIdentify() == nil || delivered.GetSetBmcIdentify().Enabled != !batch {
					t.Fatalf("unexpected delivered command: %v", delivered)
				}
			case <-stream.ctx.Done():
				t.Fatal("locator command was not delivered")
			}
			stream.incoming <- &xirangv1.AgentMessage{MessageId: "ack", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_Acknowledgement{Acknowledgement: &xirangv1.CommandAcknowledgement{CommandMessageId: action.ID, Accepted: true, Reason: "BMC acknowledged"}}}
			close(stream.incoming)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			persisted, err := health.Action(context.Background(), action.ID)
			if err != nil || persisted.Status != "succeeded" || persisted.CompletedAt == nil || persisted.Message != "BMC acknowledged" {
				t.Fatalf("acknowledgement not persisted: %+v %v", persisted, err)
			}
			reconnected := hub.Attach("node_test")
			defer reconnected.Detach()
			if command, ok := reconnected.Take(); ok {
				t.Fatalf("completed locator was replayed: %v", command)
			}
		})
	}
}

func (reporter *healthTestReporter) Report(_ context.Context, nodeID string, _ *xirangv1.HostHealthReport) error {
	reporter.nodeID = nodeID
	reporter.calls++
	return nil
}
func (*healthTestReporter) ConfirmAction(context.Context, string, string, bool, string) error {
	return nil
}

func TestHealthReportsRequireNegotiationAndUseCertificateIdentity(t *testing.T) {
	for _, negotiated := range []bool{false, true} {
		hello := helloMessage("node_test")
		if negotiated {
			hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, hosthealth.Capability)
		}
		stream := newTestStream("node_test", []*xirangv1.AgentMessage{hello, {MessageId: "health-1", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_HostHealth{HostHealth: &xirangv1.HostHealthReport{ObservedAt: timestamppb.Now()}}}})
		reporter := &healthTestReporter{}
		server := New(&testHeartbeater{}, time.Second)
		server.SetHostHealthReporter(reporter)
		err := server.Connect(stream)
		if negotiated {
			if err != nil || reporter.calls != 1 || reporter.nodeID != "node_test" {
				t.Fatalf("report delivery: %+v %v", reporter, err)
			}
		} else if status.Code(err) != codes.InvalidArgument || reporter.calls != 0 {
			t.Fatalf("unnegotiated report accepted: %+v %v", reporter, err)
		}
	}
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, hosthealth.Capability, hosthealth.IdentifyCapability)
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{hello})
	if err := New(&testHeartbeater{}, time.Second).Connect(stream); err != nil {
		t.Fatal(err)
	}
	if hasCapability(stream.sent[0].GetHelloAccepted().NegotiatedCapabilities, hosthealth.Capability) {
		t.Fatal("unconfigured controller advertised health support")
	}
}
