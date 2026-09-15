package networkhealth

import (
	"context"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type inventoryTelemetry struct {
	inventory domain.NodeInventorySnapshot
}

func (telemetry inventoryTelemetry) GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error) {
	return telemetry.inventory, nil
}

func (inventoryTelemetry) ListObservations(context.Context, string, int) ([]domain.NodeResourceObservation, error) {
	return nil, nil
}

func TestEnrichAddsInventoryCapacityAndPathSelection(t *testing.T) {
	snapshot := domain.TopologySnapshot{Edges: []domain.TopologyEdge{
		{ID: "link-10g", SourceID: "controller", TargetID: "compute", SourceInterface: "eth10"},
		{ID: "link-25g", SourceID: "controller", TargetID: "compute", SourceInterface: "eth25"},
	}}
	enriched := Enrich(context.Background(), snapshot, inventoryTelemetry{inventory: domain.NodeInventorySnapshot{NetworkInterfaces: []domain.NetworkInterfaceSnapshot{
		{Name: "eth10", SpeedBitsPerSecond: 10_000_000_000},
		{Name: "eth25", SpeedBitsPerSecond: 25_000_000_000},
	}}})
	if enriched.Edges[0].CapacityBits != 10_000_000_000 || enriched.Edges[1].CapacityBits != 25_000_000_000 {
		t.Fatalf("inventory capacities were not applied: %#v", enriched.Edges)
	}
	if len(enriched.PathSelections) != 1 || enriched.PathSelections[0].PrimaryEdgeID != "link-25g" || enriched.PathSelections[0].EffectiveCapacityBits != 25_000_000_000 {
		t.Fatalf("unexpected path selection: %#v", enriched.PathSelections)
	}
}

func TestCalculateInterfaceHealthClassifiesDrops(t *testing.T) {
	tests := []struct {
		name     string
		current  domain.NetworkInterfaceCounters
		expected string
	}{
		{name: "healthy", current: domain.NetworkInterfaceCounters{ReceivedPackets: 1_000}, expected: "healthy"},
		{name: "degraded", current: domain.NetworkInterfaceCounters{ReceivedPackets: 1_000, ReceiveDropped: 1}, expected: "degraded"},
		{name: "critical", current: domain.NetworkInterfaceCounters{ReceivedPackets: 50, ReceiveDropped: 2}, expected: "critical"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.current.PacketCountersSupported = true
			previous := domain.NetworkInterfaceCounters{PacketCountersSupported: true}
			overlay, ok := calculateInterfaceHealth(test.current, previous, time.Second.Seconds())
			if !ok || overlay.health != test.expected {
				t.Fatalf("health=%q ok=%t, expected %q", overlay.health, ok, test.expected)
			}
		})
	}
}

func TestCalculateInterfaceHealthKeepsLegacyRateWithoutHealthClaim(t *testing.T) {
	overlay, ok := calculateInterfaceHealth(
		domain.NetworkInterfaceCounters{ReceivedBytes: 200},
		domain.NetworkInterfaceCounters{ReceivedBytes: 100},
		time.Second.Seconds(),
	)
	if !ok || overlay.bitsPerSecond != 800 || overlay.health != "" {
		t.Fatalf("unexpected legacy overlay: %#v ok=%t", overlay, ok)
	}
}
