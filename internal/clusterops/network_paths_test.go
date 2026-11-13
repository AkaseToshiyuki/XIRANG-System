package clusterops

import (
	"context"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"github.com/xirang-grid/xirang/internal/networkhealth"
	"testing"
	"time"
)

type mutableTopology struct{ snapshot domain.TopologySnapshot }

func (s *mutableTopology) LatestSnapshot(context.Context) (domain.TopologySnapshot, error) {
	return s.snapshot, nil
}

func TestSharedMountUsesFreshPathAndReconcilesChangedSelection(t *testing.T) {
	now := time.Now()
	graph := &mutableTopology{snapshot: domain.TopologySnapshot{Edges: []domain.TopologyEdge{
		{ID: "a", Type: "probe", SourceID: "storage", TargetID: "compute", SourceInterface: "eth0", TargetInterface: "eth1", SourceAddress: "192.0.2.1", TargetAddress: "192.0.2.2", CapacityBits: 1000, LatencyMicros: 100, ObservedAt: now, Health: "healthy"},
		{ID: "b", Type: "probe", SourceID: "compute", TargetID: "storage", SourceInterface: "eth1", TargetInterface: "eth0", SourceAddress: "192.0.2.2", TargetAddress: "192.0.2.1", CapacityBits: 1000, LatencyMicros: 200, ObservedAt: now, Health: "healthy"},
	}}}
	nodes := memoryNodes{{ID: "storage", Status: domain.NodeStatusReady, Roles: []domain.NodeRole{domain.NodeRoleStorage}, ManagementAddress: "bootstrap"}, {ID: "compute", Status: domain.NodeStatusReady, Capabilities: []string{mountcontrol.PathCapability}}}
	runner := &recordedRunner{}
	repository := &memoryRepository{}
	service := New(repository, nodes, runner)
	service.EnableNetworkPaths(&networkhealth.Resolver{Snapshots: graph})
	space := domain.SharedSpace{ID: "share", SourceNodeID: "storage", SourcePath: "/source", MountPath: "/mount", Target: domain.SharedSpaceNodes, TargetNodeIDs: []string{"compute"}, Results: []domain.SharedSpaceNodeResult{{NodeID: "storage", Status: "exported"}, {NodeID: "compute", Status: "mounted"}}}
	nodes[1].Capabilities = nil
	retained, err := service.reconcileSpaceLocked(context.Background(), space, nodes)
	if err != nil || retained.Status != "active" || len(runner.calls) != 0 {
		t.Fatalf("rolling upgrade changed an old Agent's confirmed mount: %+v %v %v", retained, runner.calls, err)
	}
	nodes[1].Capabilities = []string{mountcontrol.PathCapability}
	updated, err := service.reconcileSpaceLocked(context.Background(), space, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0][1] != "repath" || runner.calls[0][3] != "192.0.2.1" || runner.calls[0][6] != "192.0.2.2" {
		t.Fatalf("mount ignored selected endpoints: %v", runner.calls)
	}
	runner.calls = nil
	updated, err = service.reconcileSpaceLocked(context.Background(), updated, nodes)
	if err != nil || len(runner.calls) != 0 {
		t.Fatalf("unchanged mount repeated: %v %v", runner.calls, err)
	}
	graph.snapshot.Edges[0].SourceAddress = "198.51.100.1"
	graph.snapshot.Edges[0].TargetAddress = "198.51.100.2"
	graph.snapshot.Edges[1].SourceAddress = "198.51.100.2"
	graph.snapshot.Edges[1].TargetAddress = "198.51.100.1"
	updated, err = service.reconcileSpaceLocked(context.Background(), updated, nodes)
	if err != nil || len(runner.calls) != 1 || runner.calls[0][3] != "198.51.100.1" {
		t.Fatalf("changed path not applied: %v %v", runner.calls, err)
	}
	runner.calls = nil
	graph.snapshot.Edges = nil
	updated, err = service.reconcileSpaceLocked(context.Background(), updated, nodes)
	if err != nil || updated.Status != "degraded" || len(runner.calls) != 0 {
		t.Fatalf("missing observation fabricated success or changed mount: %+v %v", updated, runner.calls)
	}
}
