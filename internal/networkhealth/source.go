package networkhealth

import (
	"context"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type SnapshotReader interface {
	LatestSnapshot(context.Context) (domain.TopologySnapshot, error)
}

// LiveSource exposes the latest immutable topology with response-local interface
// capacity and health. It is suitable for scheduler decisions but not persistence.
type LiveSource struct {
	snapshots SnapshotReader
	telemetry TelemetryReader
}

func NewLiveSource(snapshots SnapshotReader, telemetry TelemetryReader) *LiveSource {
	return &LiveSource{snapshots: snapshots, telemetry: telemetry}
}

func (source *LiveSource) LatestSnapshot(ctx context.Context) (domain.TopologySnapshot, error) {
	snapshot, err := source.snapshots.LatestSnapshot(ctx)
	if err != nil {
		return domain.TopologySnapshot{}, err
	}
	if source.telemetry != nil {
		snapshot = Enrich(ctx, snapshot, source.telemetry)
	}
	snapshot.Edges = append([]domain.TopologyEdge(nil), snapshot.Edges...)
	now := time.Now()
	for i, edge := range snapshot.Edges {
		if edge.Type == "probe" && (edge.ObservedAt.IsZero() || edge.ObservedAt.Before(now.Add(-ProbeMaxAge)) || edge.ObservedAt.After(now.Add(5*time.Second))) {
			snapshot.Edges[i].Health = "stale"
			snapshot.Edges[i].CapacityBits = 0
		}
	}
	snapshot.PathSelections = SelectParallelPaths(snapshot.Edges)
	return snapshot, nil
}
