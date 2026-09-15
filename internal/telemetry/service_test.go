package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
)

func TestServiceRejectsInvalidReports(t *testing.T) {
	repository := &memoryRepository{}
	service := NewService(repository)
	now := time.Now().UTC()
	if err := service.ReportInventory(context.Background(), "node-1", now, &xirangv1.InventorySnapshot{
		Hostname: "node-a", Architecture: "amd64", LogicalCpuCount: 8, MemoryBytes: 16 << 30,
		NetworkInterfaces: []*xirangv1.NetworkInterface{{Name: ""}},
	}); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("expected invalid inventory, got %v", err)
	}
	if err := service.ReportObservation(context.Background(), "node-1", "message-1", now.Add(10*time.Minute), &xirangv1.ResourceObservation{}); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("expected invalid future observation, got %v", err)
	}
	if err := service.ReportObservation(context.Background(), "node-1", "message-2", now, &xirangv1.ResourceObservation{
		InterfaceCounters: []*xirangv1.NetworkInterfaceCounters{
			{Name: "eth0", ReceivedBytes: 100},
			{Name: "eth0", TransmittedBytes: 200},
		},
	}); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("expected duplicate interface counter rejection, got %v", err)
	}
}

type memoryRepository struct{}

func (*memoryRepository) PutInventory(context.Context, domain.NodeInventorySnapshot) error {
	return nil
}
func (*memoryRepository) GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error) {
	return domain.NodeInventorySnapshot{}, ErrNotFound
}
func (*memoryRepository) AppendObservation(context.Context, domain.NodeResourceObservation) error {
	return nil
}
func (*memoryRepository) ListObservations(context.Context, string, int) ([]domain.NodeResourceObservation, error) {
	return nil, ErrNotFound
}
func (*memoryRepository) Close() error { return nil }
