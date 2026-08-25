package networkhealth

import (
	"context"

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
	if source.telemetry == nil {
		return snapshot, nil
	}
	return Enrich(ctx, snapshot, source.telemetry), nil
}
