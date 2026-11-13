package networkhealth

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

// ProbeMaxAge bounds operational decisions independently of snapshot retention.
const ProbeMaxAge = 95 * time.Second

// Path is a directed, reciprocal, recently observed path. Capacity is the
// conservative negotiated line rate, not a measurement of application throughput.
type Path struct {
	SourceAddress   string    `json:"sourceAddress"`
	TargetAddress   string    `json:"targetAddress"`
	SourceInterface string    `json:"sourceInterface"`
	TargetInterface string    `json:"targetInterface"`
	CapacityBits    int64     `json:"capacityBits"`
	LatencyMicros   int64     `json:"latencyMicros"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

// RankedPaths uses exactly the same ranking as placement, but operational paths
// additionally require fresh reciprocal probes and concrete IP addresses.
func RankedPaths(snapshot domain.TopologySnapshot, source, target string, now time.Time) []Path {
	edges := make([]domain.TopologyEdge, 0)
	byID := make(map[string]domain.TopologyEdge)
	for _, edge := range snapshot.Edges {
		if edge.Type != "probe" || edge.Health == "critical" || edge.Health == "stale" ||
			!((edge.SourceID == source && edge.TargetID == target) || (edge.SourceID == target && edge.TargetID == source)) ||
			edge.ObservedAt.IsZero() || edge.ObservedAt.Before(now.Add(-ProbeMaxAge)) || edge.ObservedAt.After(now.Add(5*time.Second)) {
			continue
		}
		left, a := routeAddress(edge.SourceAddress)
		right, b := routeAddress(edge.TargetAddress)
		if !a || !b || left.BitLen() != right.BitLen() || edge.SourceInterface == "" || edge.TargetInterface == "" {
			continue
		}
		edges = append(edges, edge)
		byID[edge.ID] = edge
	}
	selections := SelectParallelPaths(edges)
	if len(selections) != 1 || !selections[0].Bidirectional {
		return nil
	}
	selection := selections[0]
	forward := append([]string{selection.PrimaryEdgeID}, selection.FallbackEdgeIDs...)
	reverse := append([]string{selection.PrimaryReverseEdgeID}, selection.FallbackReverseEdgeIDs...)
	var paths []Path
	for i, id := range forward {
		a, b := byID[id], byID[reverse[i]]
		if a.ID == "" || b.ID == "" || a.SourceID == b.SourceID {
			continue
		}
		if a.SourceID != source {
			a, b = b, a
		}
		expires := a.ObservedAt
		if b.ObservedAt.Before(expires) {
			expires = b.ObservedAt
		}
		paths = append(paths, Path{SourceAddress: a.SourceAddress, TargetAddress: a.TargetAddress,
			SourceInterface: a.SourceInterface, TargetInterface: a.TargetInterface,
			CapacityBits: min(a.CapacityBits, b.CapacityBits), LatencyMicros: max(a.LatencyMicros, b.LatencyMicros),
			ExpiresAt: expires.Add(ProbeMaxAge)})
		if len(paths) == 16 {
			break
		}
	}
	return paths
}

func routeAddress(value string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(value)
	return a, err == nil && a.Zone() == "" && a.IsGlobalUnicast() && !a.IsLoopback()
}

type NodeReader interface {
	ListNodes(context.Context) ([]domain.Node, error)
}

// Resolver is shared by file access, mounts, Agent transports and operational
// clients. It never changes administrator-assigned management addresses.
type Resolver struct {
	Snapshots SnapshotReader
	Nodes     NodeReader
	LocalID   string
	Hostname  string
	Telemetry TelemetryReader
}

func (r *Resolver) LocalNodeID(ctx context.Context) (string, error) {
	if r.LocalID != "" {
		return r.LocalID, nil
	}
	nodes, err := r.Nodes.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	var id string
	for _, node := range nodes {
		if node.Name != r.Hostname || !node.HasRole(domain.NodeRoleManagement) || node.RemovedAt != nil || node.CertificateRevokedAt != nil {
			continue
		}
		if id != "" {
			return "", errors.New("controller host matches multiple enrolled nodes")
		}
		id = node.ID
	}
	if id == "" {
		return "", errors.New("controller host has no enrolled management identity")
	}
	return id, nil
}

func (r *Resolver) Paths(ctx context.Context, source, target string) ([]Path, error) {
	if source == "" {
		var err error
		source, err = r.LocalNodeID(ctx)
		if err != nil {
			return nil, err
		}
	}
	if source == target {
		return nil, nil
	}
	if r.Nodes != nil {
		nodes, err := r.Nodes.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		active := make(map[string]bool)
		for _, node := range nodes {
			active[node.ID] = node.RemovedAt == nil && node.CertificateRevokedAt == nil
		}
		if !active[source] || !active[target] {
			return nil, errors.New("network path endpoint is absent or revoked")
		}
	}
	snapshot, err := r.Snapshots.LatestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	paths := RankedPaths(snapshot, source, target, time.Now())
	if r.Telemetry != nil {
		a, err := r.Telemetry.GetInventory(ctx, source)
		if err != nil {
			return nil, err
		}
		b, err := r.Telemetry.GetInventory(ctx, target)
		if err != nil {
			return nil, err
		}
		valid := paths[:0]
		for _, path := range paths {
			if OwnsAddress(a, path.SourceInterface, path.SourceAddress) && OwnsAddress(b, path.TargetInterface, path.TargetAddress) {
				valid = append(valid, path)
			}
		}
		paths = valid
	}
	return paths, nil
}

// OwnsAddress checks exact inventory membership; it never resolves hostnames or
// assumes that two addresses on similar subnets belong to the same node.
func OwnsAddress(inventory domain.NodeInventorySnapshot, iface, address string) bool {
	for _, item := range inventory.NetworkInterfaces {
		if item.Name != iface {
			continue
		}
		for _, raw := range item.Addresses {
			if strings.Split(raw, "/")[0] == address {
				return true
			}
		}
	}
	return false
}
