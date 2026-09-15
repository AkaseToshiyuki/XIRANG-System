package flow

import (
	"context"
	"errors"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/protobuf/types/known/durationpb"
)

type memoryRepository struct{ appended domain.NodeFlowObservation }

func (repository *memoryRepository) Append(_ context.Context, item domain.NodeFlowObservation) error {
	repository.appended = item
	return nil
}
func (*memoryRepository) List(context.Context, string, int) ([]domain.NodeFlowObservation, error) {
	return nil, nil
}
func (*memoryRepository) ListLatest(context.Context) ([]domain.NodeFlowObservation, error) {
	return nil, nil
}
func (*memoryRepository) Close() error { return nil }

func TestReportValidatesAndNormalizesFlows(t *testing.T) {
	repository := &memoryRepository{}
	service := NewService(repository)
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	err := service.Report(context.Background(), "node-1", "message-1", now, &xirangv1.FlowObservation{
		Interval: durationpb.New(10 * time.Second), MapEntries: 2,
		Flows: []*xirangv1.NetworkFlow{{
			Family: "ipv6", Protocol: "tcp", Direction: "egress",
			SourceAddress: "2001:0db8::1", SourcePort: 443,
			DestinationAddress: "2001:db8::2", DestinationPort: 8443,
			Packets: 4, Bytes: 400,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := repository.appended.Flows[0].SourceAddress; got != "2001:db8::1" {
		t.Fatalf("expected canonical address, got %q", got)
	}
}

func TestReportRejectsPayloadCardinalityAndInvalidTuple(t *testing.T) {
	service := NewService(&memoryRepository{})
	service.now = func() time.Time { return time.Now().UTC() }
	input := &xirangv1.FlowObservation{
		Interval: durationpb.New(10 * time.Second),
		Flows: []*xirangv1.NetworkFlow{{
			Family: "ipv4", Protocol: "tcp", Direction: "egress",
			SourceAddress: "10.0.0.1", DestinationAddress: "2001:db8::1", Packets: 1, Bytes: 64,
		}},
	}
	if err := service.Report(context.Background(), "node-1", "message-1", time.Now(), input); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("expected invalid report, got %v", err)
	}
}
