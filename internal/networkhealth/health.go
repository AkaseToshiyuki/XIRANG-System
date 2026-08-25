// Package networkhealth derives response-local interface health from native
// cumulative counters without mutating versioned topology snapshots.
package networkhealth

import (
	"context"
	"math"

	"github.com/xirang-grid/xirang/internal/domain"
)

type TelemetryReader interface {
	GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error)
	ListObservations(context.Context, string, int) ([]domain.NodeResourceObservation, error)
}

// Enrich returns a copy of snapshot with live source-interface rate and health
// fields derived from the newest two observations.
func Enrich(ctx context.Context, snapshot domain.TopologySnapshot, telemetry TelemetryReader) domain.TopologySnapshot {
	snapshot.Edges = append([]domain.TopologyEdge(nil), snapshot.Edges...)
	overlays := map[string]map[string]interfaceHealthOverlay{}
	capacities := map[string]map[string]int64{}
	for _, edge := range snapshot.Edges {
		if _, loaded := overlays[edge.SourceID]; loaded {
			continue
		}
		overlays[edge.SourceID] = nil
		capacities[edge.SourceID] = nil
		if inventory, err := telemetry.GetInventory(ctx, edge.SourceID); err == nil {
			interfaces := make(map[string]int64, len(inventory.NetworkInterfaces))
			for _, networkInterface := range inventory.NetworkInterfaces {
				if networkInterface.SpeedBitsPerSecond <= math.MaxInt64 {
					interfaces[networkInterface.Name] = int64(networkInterface.SpeedBitsPerSecond)
				}
			}
			capacities[edge.SourceID] = interfaces
		}
		observations, err := telemetry.ListObservations(ctx, edge.SourceID, 2)
		if err != nil || len(observations) < 2 {
			continue
		}
		seconds := observations[0].ObservedAt.Sub(observations[1].ObservedAt).Seconds()
		if seconds <= 0 {
			continue
		}
		interfaceOverlays := map[string]interfaceHealthOverlay{}
		for name, current := range observations[0].InterfaceCounters {
			previous, ok := observations[1].InterfaceCounters[name]
			if !ok {
				continue
			}
			overlay, valid := calculateInterfaceHealth(current, previous, seconds)
			if valid {
				interfaceOverlays[name] = overlay
			}
		}
		overlays[edge.SourceID] = interfaceOverlays
	}
	for index := range snapshot.Edges {
		overlay := overlays[snapshot.Edges[index].SourceID][snapshot.Edges[index].SourceInterface]
		snapshot.Edges[index].ObservedBits = overlay.bitsPerSecond
		snapshot.Edges[index].ObservedPacketsPerSecond = overlay.packetsPerSecond
		snapshot.Edges[index].DroppedPacketsPerSecond = overlay.droppedPerSecond
		snapshot.Edges[index].ErrorPacketsPerSecond = overlay.errorsPerSecond
		snapshot.Edges[index].DropRatio = overlay.dropRatio
		if overlay.health != "" {
			snapshot.Edges[index].Health = overlay.health
		}
		if snapshot.Edges[index].Type != "probe" {
			snapshot.Edges[index].CapacityBits = capacities[snapshot.Edges[index].SourceID][snapshot.Edges[index].SourceInterface]
		}
	}
	snapshot.PathSelections = SelectParallelPaths(snapshot.Edges)
	return snapshot
}

type interfaceHealthOverlay struct {
	bitsPerSecond    int64
	packetsPerSecond float64
	droppedPerSecond float64
	errorsPerSecond  float64
	dropRatio        float64
	health           string
}

func calculateInterfaceHealth(current, previous domain.NetworkInterfaceCounters, seconds float64) (interfaceHealthOverlay, bool) {
	if seconds <= 0 || current.ReceivedBytes < previous.ReceivedBytes || current.TransmittedBytes < previous.TransmittedBytes {
		return interfaceHealthOverlay{}, false
	}
	bytes, ok := addCounterDeltas(current.ReceivedBytes-previous.ReceivedBytes, current.TransmittedBytes-previous.TransmittedBytes)
	if !ok || float64(bytes)*8/seconds > math.MaxInt64 {
		return interfaceHealthOverlay{}, false
	}
	overlay := interfaceHealthOverlay{bitsPerSecond: int64(float64(bytes) * 8 / seconds)}
	if !current.PacketCountersSupported || !previous.PacketCountersSupported || healthCountersRegressed(current, previous) {
		return overlay, true
	}
	packets, ok := addCounterDeltas(current.ReceivedPackets-previous.ReceivedPackets, current.TransmittedPackets-previous.TransmittedPackets)
	if !ok {
		return overlay, true
	}
	dropped, ok := addCounterDeltas(current.ReceiveDropped-previous.ReceiveDropped, current.TransmitDropped-previous.TransmitDropped)
	if !ok {
		return overlay, true
	}
	errors, ok := addCounterDeltas(current.ReceiveErrors-previous.ReceiveErrors, current.TransmitErrors-previous.TransmitErrors)
	if !ok {
		return overlay, true
	}
	dropRatio := 0.0
	if packets <= ^uint64(0)-dropped && packets+dropped > 0 {
		dropRatio = float64(dropped) / float64(packets+dropped)
	}
	health := "healthy"
	if dropped > 0 || errors > 0 {
		health = "degraded"
	}
	issues, issuesOK := addCounterDeltas(dropped, errors)
	if issuesOK && packets <= ^uint64(0)-issues && packets+issues > 0 && float64(issues)/float64(packets+issues) >= 0.01 {
		health = "critical"
	}
	overlay.packetsPerSecond = float64(packets) / seconds
	overlay.droppedPerSecond = float64(dropped) / seconds
	overlay.errorsPerSecond = float64(errors) / seconds
	overlay.dropRatio = dropRatio
	overlay.health = health
	return overlay, true
}

func healthCountersRegressed(current, previous domain.NetworkInterfaceCounters) bool {
	return current.ReceivedPackets < previous.ReceivedPackets || current.TransmittedPackets < previous.TransmittedPackets ||
		current.ReceiveErrors < previous.ReceiveErrors || current.TransmitErrors < previous.TransmitErrors ||
		current.ReceiveDropped < previous.ReceiveDropped || current.TransmitDropped < previous.TransmitDropped
}

func addCounterDeltas(left, right uint64) (uint64, bool) {
	if ^uint64(0)-left < right {
		return 0, false
	}
	return left + right, true
}
