package topology

import (
	"context"
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
)

type probeNodes struct{ nodes []domain.Node }

func (source probeNodes) ListNodes(context.Context) ([]domain.Node, error) { return source.nodes, nil }

type probeInventories map[string]domain.NodeInventorySnapshot

func (source probeInventories) GetInventory(_ context.Context, nodeID string) (domain.NodeInventorySnapshot, error) {
	return source[nodeID], nil
}

func TestProbeTargetProviderReturnsAllRoutableInterfaceAddresses(t *testing.T) {
	provider := NewProbeTargetProvider(
		probeNodes{nodes: []domain.Node{
			{ID: "node-a", Status: domain.NodeStatusReady},
			{ID: "node-b", Status: domain.NodeStatusReady},
			{ID: "node-pending", Status: domain.NodeStatusPendingApproval},
		}},
		probeInventories{
			"node-a": {NodeID: "node-a", NetworkInterfaces: []domain.NetworkInterfaceSnapshot{
				{Name: "eth0", Addresses: []string{"192.0.2.10/24", "2001:db8::10/64"}, SpeedBitsPerSecond: 25_000_000_000},
				{Name: "lo", Addresses: []string{"127.0.0.1/8"}},
			}},
			"node-b": {NodeID: "node-b", NetworkInterfaces: []domain.NetworkInterfaceSnapshot{
				{Name: "eth0", Addresses: []string{"198.51.100.20/24"}},
			}},
		},
	)
	targets, err := provider.NetworkProbeTargets(context.Background(), "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].NodeId != "node-a" || targets[0].InterfaceName != "eth0" ||
		targets[0].SpeedBitsPerSecond != 25_000_000_000 {
		t.Fatalf("unexpected targets: %#v", targets)
	}
}
