package topology

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
)

var (
	ErrNotFound      = errors.New("topology snapshot not found")
	ErrInvalidReport = errors.New("invalid topology report")
)

type SnapshotBuilder func([]domain.NodeTopologyReport, uint64, time.Time) domain.TopologySnapshot

type Repository interface {
	ApplyReport(context.Context, domain.NodeTopologyReport, SnapshotBuilder) (domain.TopologySnapshot, bool, error)
	LatestSnapshot(context.Context) (domain.TopologySnapshot, error)
	GetSnapshot(context.Context, uint64) (domain.TopologySnapshot, error)
	Close() error
}

type Service struct {
	repository Repository
	now        func() time.Time
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository, now: time.Now}
}

func (service *Service) Report(ctx context.Context, nodeID, messageID string, observedAt time.Time, observation *xirangv1.TopologyObservation) (domain.TopologySnapshot, bool, error) {
	if !validText(nodeID, 256, false) || !validText(messageID, 256, false) || observation == nil ||
		len(observation.Routes) > 4096 || len(observation.Neighbors) > 4096 || len(observation.LldpNeighbors) > 256 ||
		len(observation.PathProbes) > 4096 ||
		invalidObservedAt(observedAt, service.now()) {
		return domain.TopologySnapshot{}, false, ErrInvalidReport
	}
	report := domain.NodeTopologyReport{MessageID: messageID, NodeID: nodeID, ObservedAt: observedAt.UTC(), ReceivedAt: service.now().UTC()}
	for _, route := range observation.Routes {
		converted, ok := validatedRoute(route)
		if !ok {
			return domain.TopologySnapshot{}, false, ErrInvalidReport
		}
		report.Routes = append(report.Routes, converted)
	}
	for _, neighbor := range observation.Neighbors {
		converted, ok := validatedNeighbor(neighbor)
		if !ok {
			return domain.TopologySnapshot{}, false, ErrInvalidReport
		}
		report.Neighbors = append(report.Neighbors, converted)
	}
	for _, neighbor := range observation.LldpNeighbors {
		converted, ok := validatedLLDPNeighbor(neighbor)
		if !ok {
			return domain.TopologySnapshot{}, false, ErrInvalidReport
		}
		report.LLDPNeighbors = append(report.LLDPNeighbors, converted)
	}
	for _, probe := range observation.PathProbes {
		converted, ok := validatedPathProbe(probe, observedAt)
		if !ok {
			return domain.TopologySnapshot{}, false, ErrInvalidReport
		}
		report.PathProbes = append(report.PathProbes, converted)
	}
	sort.Slice(report.Routes, func(left, right int) bool { return routeKey(report.Routes[left]) < routeKey(report.Routes[right]) })
	sort.Slice(report.Neighbors, func(left, right int) bool {
		return neighborKey(report.Neighbors[left]) < neighborKey(report.Neighbors[right])
	})
	sort.Slice(report.LLDPNeighbors, func(left, right int) bool {
		return lldpKey(report.LLDPNeighbors[left]) < lldpKey(report.LLDPNeighbors[right])
	})
	sort.Slice(report.PathProbes, func(left, right int) bool {
		return pathProbeKey(report.PathProbes[left]) < pathProbeKey(report.PathProbes[right])
	})
	report.ContentHash = reportHash(report)
	return service.repository.ApplyReport(ctx, report, buildSnapshot)
}

func (service *Service) LatestSnapshot(ctx context.Context) (domain.TopologySnapshot, error) {
	return service.repository.LatestSnapshot(ctx)
}

func (service *Service) GetSnapshot(ctx context.Context, version uint64) (domain.TopologySnapshot, error) {
	return service.repository.GetSnapshot(ctx, version)
}

func validatedRoute(route *xirangv1.Route) (domain.TopologyRoute, bool) {
	if route == nil || !validText(route.InterfaceName, 64, false) || (route.Family != "ipv4" && route.Family != "ipv6") || route.Table > 1<<24 {
		return domain.TopologyRoute{}, false
	}
	destination, err := netip.ParsePrefix(route.Destination)
	if err != nil || (route.Family == "ipv4") != destination.Addr().Is4() {
		return domain.TopologyRoute{}, false
	}
	gateway := ""
	if route.Gateway != "" {
		address, err := netip.ParseAddr(route.Gateway)
		if err != nil || (route.Family == "ipv4") != address.Is4() {
			return domain.TopologyRoute{}, false
		}
		gateway = address.String()
	}
	return domain.TopologyRoute{
		Destination: destination.Masked().String(), Gateway: gateway, InterfaceName: route.InterfaceName,
		Metric: route.Metric, Table: route.Table, Family: route.Family,
	}, true
}

func validatedNeighbor(neighbor *xirangv1.Neighbor) (domain.TopologyNeighbor, bool) {
	if neighbor == nil || !validText(neighbor.InterfaceName, 64, false) || !validText(neighbor.State, 32, false) {
		return domain.TopologyNeighbor{}, false
	}
	address, err := netip.ParseAddr(neighbor.Address)
	if err != nil {
		return domain.TopologyNeighbor{}, false
	}
	mac := ""
	if neighbor.MacAddress != "" {
		hardware, err := net.ParseMAC(neighbor.MacAddress)
		if err != nil {
			return domain.TopologyNeighbor{}, false
		}
		mac = strings.ToLower(hardware.String())
	}
	return domain.TopologyNeighbor{
		Address: address.String(), MACAddress: mac, InterfaceName: neighbor.InterfaceName,
		State: strings.ToLower(neighbor.State), Router: neighbor.Router,
	}, true
}

func validatedLLDPNeighbor(neighbor *xirangv1.LLDPNeighbor) (domain.TopologyLLDPNeighbor, bool) {
	if neighbor == nil || !validText(neighbor.LocalInterface, 64, false) || !validText(neighbor.ChassisId, 256, false) ||
		!validText(neighbor.PortId, 256, true) || !validText(neighbor.SystemName, 256, true) ||
		!validText(neighbor.SystemDescription, 2048, true) || len(neighbor.ManagementAddresses) > 32 {
		return domain.TopologyLLDPNeighbor{}, false
	}
	addresses := make([]string, 0, len(neighbor.ManagementAddresses))
	for _, raw := range neighbor.ManagementAddresses {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			return domain.TopologyLLDPNeighbor{}, false
		}
		addresses = append(addresses, address.String())
	}
	sort.Strings(addresses)
	return domain.TopologyLLDPNeighbor{
		LocalInterface: neighbor.LocalInterface, ChassisID: neighbor.ChassisId, PortID: neighbor.PortId,
		SystemName: neighbor.SystemName, SystemDescription: neighbor.SystemDescription, ManagementAddresses: addresses,
	}, true
}

func validatedPathProbe(probe *xirangv1.NetworkPathProbe, observedAt time.Time) (domain.NetworkPathProbe, bool) {
	if probe == nil || !validText(probe.TargetNodeId, 256, false) ||
		!validText(probe.SourceInterface, 64, false) || !validText(probe.TargetInterface, 64, false) ||
		!validText(probe.Failure, 128, true) || probe.LatencyMicros < 0 || probe.LatencyMicros > int64((time.Minute)/time.Microsecond) ||
		probe.SourceSpeedBitsPerSecond > 1_000_000_000_000_000 || probe.TargetSpeedBitsPerSecond > 1_000_000_000_000_000 {
		return domain.NetworkPathProbe{}, false
	}
	source, sourceErr := netip.ParseAddr(probe.SourceAddress)
	target, targetErr := netip.ParseAddr(probe.TargetAddress)
	if sourceErr != nil || targetErr != nil || source.BitLen() != target.BitLen() ||
		(probe.Reachable && probe.LatencyMicros <= 0) || (!probe.Reachable && probe.LatencyMicros != 0) {
		return domain.NetworkPathProbe{}, false
	}
	return domain.NetworkPathProbe{
		TargetNodeID:    probe.TargetNodeId,
		SourceInterface: probe.SourceInterface, SourceAddress: source.Unmap().String(),
		TargetInterface: probe.TargetInterface, TargetAddress: target.Unmap().String(),
		SourceSpeedBitsPerSecond: probe.SourceSpeedBitsPerSecond,
		TargetSpeedBitsPerSecond: probe.TargetSpeedBitsPerSecond,
		LatencyMicros:            probe.LatencyMicros, Reachable: probe.Reachable,
		Failure: probe.Failure, ObservedAt: observedAt.UTC(),
	}, true
}

func buildSnapshot(reports []domain.NodeTopologyReport, version uint64, generatedAt time.Time) domain.TopologySnapshot {
	snapshot := domain.TopologySnapshot{Version: version, GeneratedAt: generatedAt.UTC(), ReportCount: len(reports)}
	vertices := map[string]domain.TopologyVertex{}
	edges := map[string]domain.TopologyEdge{}
	sort.Slice(reports, func(left, right int) bool { return reports[left].NodeID < reports[right].NodeID })
	for _, report := range reports {
		vertices[report.NodeID] = domain.TopologyVertex{ID: report.NodeID, Kind: domain.TopologyNodeHost, Labels: map[string]string{"nodeId": report.NodeID}}
		neighborTargets := map[string]string{}
		for _, neighbor := range report.Neighbors {
			neighbor.NodeID = report.NodeID
			snapshot.Neighbors = append(snapshot.Neighbors, neighbor)
			targetID := externalVertexID("neighbor", firstNonEmpty(neighbor.MACAddress, neighbor.Address))
			neighborTargets[neighbor.Address] = targetID
			kind := domain.TopologyNodeUnknown
			if neighbor.Router {
				kind = domain.TopologyNodeRouter
			}
			vertices[targetID] = domain.TopologyVertex{ID: targetID, Kind: kind, Labels: compactLabels(map[string]string{
				"address": neighbor.Address, "macAddress": neighbor.MACAddress, "state": neighbor.State,
			})}
			edge := domain.TopologyEdge{
				ID: edgeID("neighbor", report.NodeID, targetID, neighbor.InterfaceName), Type: "neighbor",
				SourceID: report.NodeID, TargetID: targetID, SourceInterface: neighbor.InterfaceName, ObservedAt: report.ObservedAt,
			}
			edges[edge.ID] = edge
		}
		for _, neighbor := range report.LLDPNeighbors {
			neighbor.NodeID = report.NodeID
			snapshot.LLDP = append(snapshot.LLDP, neighbor)
			targetID := externalVertexID("lldp", neighbor.ChassisID)
			vertices[targetID] = domain.TopologyVertex{ID: targetID, Kind: domain.TopologyNodeSwitch, Labels: compactLabels(map[string]string{
				"chassisId": neighbor.ChassisID, "systemName": neighbor.SystemName,
				"systemDescription": neighbor.SystemDescription, "managementAddresses": strings.Join(neighbor.ManagementAddresses, ","),
			})}
			edge := domain.TopologyEdge{
				ID: edgeID("lldp", report.NodeID, targetID, neighbor.LocalInterface), Type: "lldp",
				SourceID: report.NodeID, TargetID: targetID, SourceInterface: neighbor.LocalInterface,
				TargetInterface: neighbor.PortID, ObservedAt: report.ObservedAt,
			}
			edges[edge.ID] = edge
		}
		for _, route := range report.Routes {
			route.NodeID = report.NodeID
			snapshot.Routes = append(snapshot.Routes, route)
			if route.Gateway == "" {
				continue
			}
			targetID := neighborTargets[route.Gateway]
			if targetID == "" {
				targetID = externalVertexID("gateway", route.Gateway)
				vertices[targetID] = domain.TopologyVertex{ID: targetID, Kind: domain.TopologyNodeRouter, Labels: map[string]string{"address": route.Gateway}}
			}
			edge := domain.TopologyEdge{
				ID: edgeID("route", report.NodeID, targetID, route.InterfaceName), Type: "route",
				SourceID: report.NodeID, TargetID: targetID, SourceInterface: route.InterfaceName, ObservedAt: report.ObservedAt,
			}
			edges[edge.ID] = edge
		}
		for _, probe := range report.PathProbes {
			probe.NodeID = report.NodeID
			snapshot.PathProbes = append(snapshot.PathProbes, probe)
			if !probe.Reachable {
				continue
			}
			vertices[probe.TargetNodeID] = domain.TopologyVertex{
				ID: probe.TargetNodeID, Kind: domain.TopologyNodeHost,
				Labels: map[string]string{"nodeId": probe.TargetNodeID},
			}
			var capacity int64
			if probe.SourceSpeedBitsPerSecond > 0 && probe.TargetSpeedBitsPerSecond > 0 {
				capacity = int64(min(probe.SourceSpeedBitsPerSecond, probe.TargetSpeedBitsPerSecond))
			}
			edge := domain.TopologyEdge{
				ID: edgeID("probe", report.NodeID, probe.TargetNodeID, strings.Join([]string{
					probe.SourceInterface, probe.SourceAddress, probe.TargetInterface, probe.TargetAddress,
				}, "\x00")),
				Type: "probe", SourceID: report.NodeID, TargetID: probe.TargetNodeID,
				SourceInterface: probe.SourceInterface, TargetInterface: probe.TargetInterface,
				SourceAddress: probe.SourceAddress, TargetAddress: probe.TargetAddress,
				CapacityBits: capacity, LatencyMicros: probe.LatencyMicros,
				Health: "healthy", ObservedAt: probe.ObservedAt,
			}
			edges[edge.ID] = edge
		}
	}
	for _, vertex := range vertices {
		snapshot.Vertices = append(snapshot.Vertices, vertex)
	}
	for _, edge := range edges {
		snapshot.Edges = append(snapshot.Edges, edge)
	}
	sort.Slice(snapshot.Vertices, func(left, right int) bool { return snapshot.Vertices[left].ID < snapshot.Vertices[right].ID })
	sort.Slice(snapshot.Edges, func(left, right int) bool { return snapshot.Edges[left].ID < snapshot.Edges[right].ID })
	sort.Slice(snapshot.Routes, func(left, right int) bool {
		return snapshot.Routes[left].NodeID+routeKey(snapshot.Routes[left]) < snapshot.Routes[right].NodeID+routeKey(snapshot.Routes[right])
	})
	sort.Slice(snapshot.Neighbors, func(left, right int) bool {
		return snapshot.Neighbors[left].NodeID+neighborKey(snapshot.Neighbors[left]) < snapshot.Neighbors[right].NodeID+neighborKey(snapshot.Neighbors[right])
	})
	sort.Slice(snapshot.LLDP, func(left, right int) bool {
		return snapshot.LLDP[left].NodeID+lldpKey(snapshot.LLDP[left]) < snapshot.LLDP[right].NodeID+lldpKey(snapshot.LLDP[right])
	})
	sort.Slice(snapshot.PathProbes, func(left, right int) bool {
		return snapshot.PathProbes[left].NodeID+pathProbeKey(snapshot.PathProbes[left]) <
			snapshot.PathProbes[right].NodeID+pathProbeKey(snapshot.PathProbes[right])
	})
	return snapshot
}

func reportHash(report domain.NodeTopologyReport) string {
	encoded, _ := json.Marshal(struct {
		Routes        []domain.TopologyRoute
		Neighbors     []domain.TopologyNeighbor
		LLDPNeighbors []domain.TopologyLLDPNeighbor
		PathProbes    []domain.NetworkPathProbe
	}{report.Routes, report.Neighbors, report.LLDPNeighbors, report.PathProbes})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func externalVertexID(kind, identity string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + identity))
	return kind + "_" + hex.EncodeToString(digest[:12])
}

func edgeID(kind, source, target, localInterface string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{kind, source, target, localInterface}, "\x00")))
	return "edge_" + hex.EncodeToString(digest[:12])
}

func compactLabels(labels map[string]string) map[string]string {
	for key, value := range labels {
		if value == "" {
			delete(labels, key)
		}
	}
	return labels
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
}

func validText(value string, maximum int, emptyAllowed bool) bool {
	return (emptyAllowed || value != "") && len(value) <= maximum && !strings.ContainsRune(value, '\x00')
}

func invalidObservedAt(observedAt, now time.Time) bool {
	return observedAt.IsZero() || observedAt.Before(now.Add(-24*time.Hour)) || observedAt.After(now.Add(5*time.Minute))
}

func routeKey(route domain.TopologyRoute) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%08x\x00%08x", route.Family, route.Destination, route.Gateway, route.InterfaceName, route.Table, route.Metric)
}

func neighborKey(neighbor domain.TopologyNeighbor) string {
	return strings.Join([]string{neighbor.InterfaceName, neighbor.Address, neighbor.MACAddress, neighbor.State}, "\x00")
}

func lldpKey(neighbor domain.TopologyLLDPNeighbor) string {
	return strings.Join([]string{neighbor.LocalInterface, neighbor.ChassisID, neighbor.PortID}, "\x00")
}

func pathProbeKey(probe domain.NetworkPathProbe) string {
	return strings.Join([]string{
		probe.TargetNodeID, probe.SourceInterface, probe.SourceAddress,
		probe.TargetInterface, probe.TargetAddress,
	}, "\x00")
}
