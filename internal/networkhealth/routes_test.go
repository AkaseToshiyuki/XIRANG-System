package networkhealth

import (
	"github.com/xirang-grid/xirang/internal/domain"
	"testing"
	"time"
)

func routeFixture(now time.Time) domain.TopologySnapshot {
	return domain.TopologySnapshot{Edges: []domain.TopologyEdge{
		{ID: "fast-a", Type: "probe", SourceID: "a", TargetID: "b", SourceInterface: "eth1", TargetInterface: "eno1", SourceAddress: "192.0.2.1", TargetAddress: "192.0.2.2", CapacityBits: 25000000000, LatencyMicros: 500, Health: "healthy", ObservedAt: now},
		{ID: "fast-b", Type: "probe", SourceID: "b", TargetID: "a", SourceInterface: "eno1", TargetInterface: "eth1", SourceAddress: "192.0.2.2", TargetAddress: "192.0.2.1", CapacityBits: 10000000000, LatencyMicros: 600, Health: "healthy", ObservedAt: now},
		{ID: "slow-a", Type: "probe", SourceID: "a", TargetID: "b", SourceInterface: "eth2", TargetInterface: "eno2", SourceAddress: "198.51.100.1", TargetAddress: "198.51.100.2", CapacityBits: 1000000000, LatencyMicros: 100, Health: "healthy", ObservedAt: now},
		{ID: "slow-b", Type: "probe", SourceID: "b", TargetID: "a", SourceInterface: "eno2", TargetInterface: "eth2", SourceAddress: "198.51.100.2", TargetAddress: "198.51.100.1", CapacityBits: 1000000000, LatencyMicros: 200, Health: "healthy", ObservedAt: now},
	}}
}

func TestOperationalPathsRankCapacityBeforeLatencyAndOrientClient(t *testing.T) {
	now := time.Now()
	snapshot := routeFixture(now)
	paths := RankedPaths(snapshot, "b", "a", now)
	if len(paths) != 2 || paths[0].SourceAddress != "192.0.2.2" || paths[0].TargetAddress != "192.0.2.1" || paths[0].SourceInterface != "eno1" || paths[0].CapacityBits != 10000000000 || paths[0].LatencyMicros != 600 {
		t.Fatalf("unexpected directed conservative ranking: %+v", paths)
	}
	snapshot.Edges[0].CapacityBits = 1000000000
	snapshot.Edges[1].CapacityBits = 1000000000
	if paths = RankedPaths(snapshot, "a", "b", now); paths[0].SourceInterface != "eth2" {
		t.Fatalf("equal capacity did not choose lowest measured latency: %+v", paths)
	}
}

func TestOperationalPathsReevaluateLostRecoveredStaleAndOneWayLinks(t *testing.T) {
	now := time.Now()
	for _, failure := range []string{"critical", "stale", "missing", "expired", "future", "alias"} {
		t.Run(failure, func(t *testing.T) {
			s := routeFixture(now)
			switch failure {
			case "critical", "stale":
				s.Edges[0].Health = failure
			case "missing":
				s.Edges = s.Edges[1:]
			case "expired":
				s.Edges[0].ObservedAt = now.Add(-2 * time.Minute)
			case "future":
				s.Edges[0].ObservedAt = now.Add(time.Minute)
			case "alias":
				s.Edges[1].TargetAddress = "192.0.2.9"
			}
			paths := RankedPaths(s, "a", "b", now)
			if len(paths) != 1 || paths[0].SourceInterface != "eth2" {
				t.Fatalf("failed path remained usable: %+v", paths)
			}
			paths = RankedPaths(routeFixture(now), "a", "b", now)
			if paths[0].SourceInterface != "eth1" {
				t.Fatal("restored faster link was not reselected")
			}
		})
	}
}

func TestKernelRouteMustMatchMeasuredInterface(t *testing.T) {
	if err := CheckRouteJSON([]byte(`[{"dev":"eno2","prefsrc":"192.0.2.1"}]`), "eno2"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`[{"dev":"tailscale0"}]`, `[{"dev":"eno2","type":"blackhole"}]`, `[]`, `[{"dev":"eno2"},{"dev":"eno2"}]`, `not json`} {
		if CheckRouteJSON([]byte(raw), "eno2") == nil {
			t.Fatalf("accepted invalid kernel route %s", raw)
		}
	}
}

func TestReciprocalAliasesStableAcrossProbeJitterAndLostAddress(t *testing.T) {
	now := time.Now()
	s := routeFixture(now)
	v6a, v6b := s.Edges[0], s.Edges[1]
	v6a.ID, v6b.ID = "alias-a", "alias-b"
	v6a.SourceAddress, v6a.TargetAddress = "fd00::1", "fd00::2"
	v6b.SourceAddress, v6b.TargetAddress = "fd00::2", "fd00::1"
	s.Edges = append(s.Edges, v6a, v6b)
	for _, sample := range []struct {
		ipv4, ipv6 int64
		selected   string
	}{
		{80, 100, "192.0.2.1"},
		{190, 85, "192.0.2.1"},
		{700, 89, "fd00::1"},
	} {
		s.Edges[0].LatencyMicros, s.Edges[1].LatencyMicros = sample.ipv4, sample.ipv4
		s.Edges[4].LatencyMicros, s.Edges[5].LatencyMicros = sample.ipv6, sample.ipv6
		fwd, rev := RankedPaths(s, "a", "b", now), RankedPaths(s, "b", "a", now)
		if len(fwd) != 2 || len(rev) != 2 || fwd[0].SourceAddress != sample.selected || rev[0].TargetAddress != sample.selected {
			t.Fatalf("unexpected selection at %+v: %+v, %+v", sample, fwd, rev)
		}
	}
	s.Edges[4].Health = "critical"
	if got := RankedPaths(s, "a", "b", now); len(got) != 2 || got[0].SourceAddress != "192.0.2.1" {
		t.Fatalf("lost alias retained: %+v", got)
	}
}
