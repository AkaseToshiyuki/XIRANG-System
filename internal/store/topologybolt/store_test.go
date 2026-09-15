package topologybolt_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	topologybolt "github.com/xirang-grid/xirang/internal/store/topologybolt"
	"github.com/xirang-grid/xirang/internal/topology"
)

func TestStoreVersionsDeduplicatesAndBoundsSnapshots(t *testing.T) {
	store, err := topologybolt.Open(filepath.Join(t.TempDir(), "topology.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := topology.NewService(store)
	base := time.Now().UTC().Add(-time.Minute)
	report := func(message, gateway string, second int) (uint64, bool) {
		t.Helper()
		snapshot, changed, err := service.Report(context.Background(), "node-1", message, base.Add(time.Duration(second)*time.Second), &xirangv1.TopologyObservation{
			Routes:    []*xirangv1.Route{{Destination: "0.0.0.0/0", Gateway: gateway, InterfaceName: "eth0", Table: 254, Family: "ipv4"}},
			Neighbors: []*xirangv1.Neighbor{{Address: gateway, MacAddress: "00:11:22:33:44:55", InterfaceName: "eth0", State: "reachable", Router: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return snapshot.Version, changed
	}
	if version, changed := report("message-1", "10.0.0.1", 1); version != 1 || !changed {
		t.Fatalf("unexpected initial snapshot version=%d changed=%t", version, changed)
	}
	if version, changed := report("message-duplicate", "10.0.0.1", 2); version != 1 || changed {
		t.Fatalf("duplicate topology created a version=%d changed=%t", version, changed)
	}
	if version, changed := report("message-2", "10.0.0.2", 3); version != 2 || !changed {
		t.Fatalf("unexpected second snapshot version=%d changed=%t", version, changed)
	}
	if version, changed := report("message-3", "10.0.0.3", 4); version != 3 || !changed {
		t.Fatalf("unexpected third snapshot version=%d changed=%t", version, changed)
	}
	latest, err := service.LatestSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 3 || latest.ReportCount != 1 || len(latest.Vertices) != 2 || len(latest.Edges) != 2 {
		t.Fatalf("unexpected latest snapshot: %#v", latest)
	}
	if _, err := service.GetSnapshot(context.Background(), 1); !errors.Is(err, topology.ErrNotFound) {
		t.Fatalf("expected oldest snapshot eviction, got %v", err)
	}
	if snapshot, err := service.GetSnapshot(context.Background(), 2); err != nil || snapshot.Version != 2 {
		t.Fatalf("retained snapshot unavailable: version=%d err=%v", snapshot.Version, err)
	}
}
