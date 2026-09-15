package networkhealth

import (
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestSelectParallelPathsUsesBestLinkWithoutAggregating(t *testing.T) {
	edges := []domain.TopologyEdge{
		{ID: "link-10g", SourceID: "controller", TargetID: "compute", CapacityBits: 10_000_000_000, Health: "healthy"},
		{ID: "link-25g", SourceID: "controller", TargetID: "compute", CapacityBits: 25_000_000_000, Health: "healthy"},
	}
	selections := SelectParallelPaths(edges)
	if len(selections) != 1 || selections[0].PrimaryEdgeID != "link-25g" || selections[0].EffectiveCapacityBits != 25_000_000_000 {
		t.Fatalf("expected 25G primary without aggregation, got %#v", selections)
	}
	if len(selections[0].FallbackEdgeIDs) != 1 || selections[0].FallbackEdgeIDs[0] != "link-10g" {
		t.Fatalf("expected 10G fallback, got %#v", selections[0].FallbackEdgeIDs)
	}
}

func TestSelectParallelPathsFallsBackWhenFastLinkIsCritical(t *testing.T) {
	edges := []domain.TopologyEdge{
		{ID: "link-10g", SourceID: "controller", TargetID: "compute", CapacityBits: 10_000_000_000, Health: "healthy"},
		{ID: "link-25g", SourceID: "controller", TargetID: "compute", CapacityBits: 25_000_000_000, Health: "critical"},
	}
	selections := SelectParallelPaths(edges)
	if len(selections) != 1 || selections[0].PrimaryEdgeID != "link-10g" || selections[0].EffectiveCapacityBits != 10_000_000_000 {
		t.Fatalf("expected automatic 10G fallback, got %#v", selections)
	}
	if len(selections[0].FallbackEdgeIDs) != 0 {
		t.Fatalf("critical 25G must not remain a fallback, got %#v", selections[0].FallbackEdgeIDs)
	}
}

func TestSelectParallelPathsKeepsSingleRemainingLinkAfterDisappearance(t *testing.T) {
	selections := SelectParallelPaths([]domain.TopologyEdge{{
		ID: "link-10g", SourceID: "controller", TargetID: "compute", CapacityBits: 10_000_000_000, Health: "healthy",
	}})
	if len(selections) != 1 || selections[0].PrimaryEdgeID != "link-10g" {
		t.Fatalf("expected remaining 10G link to stay selected, got %#v", selections)
	}
}

func TestSelectParallelPathsDoesNotTreatSameInterfaceFactsAsIndependentLinks(t *testing.T) {
	selections := SelectParallelPaths([]domain.TopologyEdge{
		{ID: "neighbor-eth0", Type: "neighbor", SourceID: "controller", TargetID: "compute", SourceInterface: "eth0", CapacityBits: 25_000_000_000},
		{ID: "route-eth0", Type: "route", SourceID: "controller", TargetID: "compute", SourceInterface: "eth0", CapacityBits: 25_000_000_000},
	})
	if len(selections) != 1 || len(selections[0].FallbackEdgeIDs) != 0 {
		t.Fatalf("same physical interface was exposed as a fallback: %#v", selections)
	}
}

func TestSelectParallelPathsUsesLowestLatencyWhenCapacityMatches(t *testing.T) {
	selections := SelectParallelPaths([]domain.TopologyEdge{
		{ID: "slow", SourceID: "node-a", TargetID: "node-b", SourceInterface: "eth0", TargetInterface: "eno1", CapacityBits: 25_000_000_000, LatencyMicros: 900, Health: "healthy"},
		{ID: "fast", SourceID: "node-a", TargetID: "node-b", SourceInterface: "eth1", TargetInterface: "eno2", CapacityBits: 25_000_000_000, LatencyMicros: 300, Health: "healthy"},
	})
	if len(selections) != 1 || selections[0].PrimaryEdgeID != "fast" {
		t.Fatalf("expected lower-latency equal-capacity link, got %#v", selections)
	}
}

func TestSelectParallelPathsCollapsesAddressPairsOnOneInterface(t *testing.T) {
	selections := SelectParallelPaths([]domain.TopologyEdge{
		{ID: "lan-ipv6-a", Type: "probe", SourceID: "node-a", TargetID: "node-b", SourceInterface: "eth0", SourceAddress: "2001:db8::10", TargetInterface: "eno1", TargetAddress: "2001:db8::20", CapacityBits: 25_000_000_000, LatencyMicros: 400, Health: "healthy"},
		{ID: "lan-ipv6-b", Type: "probe", SourceID: "node-a", TargetID: "node-b", SourceInterface: "eth0", SourceAddress: "2001:db8::11", TargetInterface: "eno1", TargetAddress: "2001:db8::21", CapacityBits: 25_000_000_000, LatencyMicros: 300, Health: "healthy"},
		{ID: "overlay", Type: "probe", SourceID: "node-a", TargetID: "node-b", SourceInterface: "tun0", SourceAddress: "198.51.100.10", TargetInterface: "tun0", TargetAddress: "198.51.100.20", LatencyMicros: 800, Health: "healthy"},
	})
	if len(selections) != 1 || selections[0].PrimaryEdgeID != "lan-ipv6-b" ||
		len(selections[0].FallbackEdgeIDs) != 1 || selections[0].FallbackEdgeIDs[0] != "overlay" {
		t.Fatalf("address aliases inflated physical fallback links: %#v", selections)
	}
}

func TestSelectParallelPathsUsesConservativeBidirectionalMetrics(t *testing.T) {
	selections := SelectParallelPaths([]domain.TopologyEdge{
		{ID: "a-to-b", Type: "probe", SourceID: "node-a", TargetID: "node-b", SourceInterface: "eth0", SourceAddress: "192.0.2.10", TargetInterface: "eno1", TargetAddress: "192.0.2.20", CapacityBits: 25_000_000_000, LatencyMicros: 200, Health: "healthy"},
		{ID: "b-to-a", Type: "probe", SourceID: "node-b", TargetID: "node-a", SourceInterface: "eno1", SourceAddress: "192.0.2.20", TargetInterface: "eth0", TargetAddress: "192.0.2.10", CapacityBits: 10_000_000_000, LatencyMicros: 700, Health: "degraded"},
	})
	if len(selections) != 1 || !selections[0].Bidirectional || selections[0].EffectiveCapacityBits != 10_000_000_000 ||
		selections[0].EffectiveLatencyMicros != 700 || selections[0].Health != "degraded" {
		t.Fatalf("unexpected conservative path: %#v", selections)
	}
}

func TestSelectParallelPathsDoesNotTreatOneWayProbeAsCapacity(t *testing.T) {
	selections := SelectParallelPaths([]domain.TopologyEdge{{
		ID: "a-to-b", Type: "probe", SourceID: "node-a", TargetID: "node-b", SourceInterface: "eth0", TargetInterface: "eno1", CapacityBits: 25_000_000_000, LatencyMicros: 200, Health: "healthy",
	}})
	if len(selections) != 1 || selections[0].Bidirectional || selections[0].EffectiveCapacityBits != 0 {
		t.Fatalf("one-way evidence became schedulable capacity: %#v", selections)
	}
}

func TestSelectParallelPathsRequiresReciprocalAddressPair(t *testing.T) {
	selections := SelectParallelPaths([]domain.TopologyEdge{
		{ID: "a1-to-b1", Type: "probe", SourceID: "node-a", TargetID: "node-b", SourceInterface: "eth0", SourceAddress: "192.0.2.10", TargetInterface: "eno1", TargetAddress: "192.0.2.20", CapacityBits: 25, LatencyMicros: 200, Health: "healthy"},
		{ID: "b2-to-a2", Type: "probe", SourceID: "node-b", TargetID: "node-a", SourceInterface: "eno1", SourceAddress: "192.0.2.21", TargetInterface: "eth0", TargetAddress: "192.0.2.11", CapacityBits: 25, LatencyMicros: 200, Health: "healthy"},
	})
	if len(selections) != 1 || selections[0].Bidirectional || selections[0].EffectiveCapacityBits != 0 {
		t.Fatalf("different address pairs were combined as reciprocal evidence: %#v", selections)
	}
}
