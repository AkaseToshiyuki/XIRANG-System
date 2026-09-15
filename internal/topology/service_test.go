package topology

import (
	"context"
	"errors"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
)

func TestServiceRejectsMalformedTopology(t *testing.T) {
	service := NewService(&testRepository{})
	_, _, err := service.Report(context.Background(), "node-1", "message-1", time.Now(), &xirangv1.TopologyObservation{
		Routes: []*xirangv1.Route{{Destination: "not-a-prefix", InterfaceName: "eth0", Table: 254, Family: "ipv4"}},
	})
	if !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("expected invalid report, got %v", err)
	}
}

func TestBuildSnapshotConnectsGatewayToKnownNeighbor(t *testing.T) {
	report := domain.NodeTopologyReport{
		NodeID: "node-1", ObservedAt: time.Now().UTC(),
		Routes:        []domain.TopologyRoute{{Destination: "0.0.0.0/0", Gateway: "10.0.0.1", InterfaceName: "eth0", Table: 254, Family: "ipv4"}},
		Neighbors:     []domain.TopologyNeighbor{{Address: "10.0.0.1", MACAddress: "00:11:22:33:44:55", InterfaceName: "eth0", State: "reachable", Router: true}},
		LLDPNeighbors: []domain.TopologyLLDPNeighbor{{LocalInterface: "eth0", ChassisID: "leaf-a", PortID: "Ethernet1", SystemName: "leaf-a"}},
	}
	snapshot := buildSnapshot([]domain.NodeTopologyReport{report}, 7, time.Now())
	if snapshot.Version != 7 || len(snapshot.Vertices) != 3 || len(snapshot.Edges) != 3 || len(snapshot.Routes) != 1 || len(snapshot.LLDP) != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
}

func TestBuildSnapshotIncludesNetworkPathProbe(t *testing.T) {
	observedAt := time.Now().UTC()
	snapshot := buildSnapshot([]domain.NodeTopologyReport{{
		NodeID: "node-a", ObservedAt: observedAt,
		PathProbes: []domain.NetworkPathProbe{{
			TargetNodeID: "node-b", SourceInterface: "eth0", SourceAddress: "192.0.2.10",
			TargetInterface: "eno1", TargetAddress: "192.0.2.20",
			SourceSpeedBitsPerSecond: 25_000_000_000, TargetSpeedBitsPerSecond: 10_000_000_000,
			LatencyMicros: 420, Reachable: true, ObservedAt: observedAt,
		}},
	}}, 8, observedAt)
	if len(snapshot.PathProbes) != 1 || len(snapshot.Edges) != 1 || snapshot.PathProbes[0].NodeID != "node-a" ||
		snapshot.PathProbes[0].TargetNodeID != "node-b" || snapshot.PathProbes[0].LatencyMicros != 420 {
		t.Fatalf("unexpected path probes: %#v", snapshot.PathProbes)
	}
	if snapshot.Edges[0].Type != "probe" || snapshot.Edges[0].CapacityBits != 10_000_000_000 ||
		snapshot.Edges[0].LatencyMicros != 420 {
		t.Fatalf("unexpected probe edge: %#v", snapshot.Edges[0])
	}
}

type testRepository struct{}

func (*testRepository) ApplyReport(context.Context, domain.NodeTopologyReport, SnapshotBuilder) (domain.TopologySnapshot, bool, error) {
	return domain.TopologySnapshot{}, false, nil
}
func (*testRepository) LatestSnapshot(context.Context) (domain.TopologySnapshot, error) {
	return domain.TopologySnapshot{}, ErrNotFound
}
func (*testRepository) GetSnapshot(context.Context, uint64) (domain.TopologySnapshot, error) {
	return domain.TopologySnapshot{}, ErrNotFound
}
func (*testRepository) Close() error { return nil }
