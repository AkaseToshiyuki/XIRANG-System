package controlstream

import (
	"context"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type healthTestReporter struct {
	nodeID string
	calls  int
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
