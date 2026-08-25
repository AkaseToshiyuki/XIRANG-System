package alerting

import (
	"context"
	"errors"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/networkhealth"
	"github.com/xirang-grid/xirang/internal/topology"
)

type TopologyReader interface {
	LatestSnapshot(context.Context) (domain.TopologySnapshot, error)
}

func RunEvaluator(ctx context.Context, topologyReader TopologyReader, telemetryReader networkhealth.TelemetryReader, service *Service, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	initialized := false
	evaluate := func() {
		if !initialized {
			if err := service.ResetPending(ctx); err != nil {
				if onError != nil && !errors.Is(err, context.Canceled) {
					onError(err)
				}
				return
			}
			initialized = true
		}
		snapshot, err := topologyReader.LatestSnapshot(ctx)
		if errors.Is(err, topology.ErrNotFound) || errors.Is(err, context.Canceled) {
			return
		}
		if err == nil {
			snapshot = networkhealth.Enrich(ctx, snapshot, telemetryReader)
			_, err = service.Evaluate(ctx, snapshot)
		}
		if err != nil && onError != nil && !errors.Is(err, context.Canceled) {
			onError(err)
		}
	}
	evaluate()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			evaluate()
		}
	}
}
