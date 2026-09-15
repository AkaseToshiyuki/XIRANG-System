package networkhealth

import (
	"math"
	"sort"
	"strings"

	"github.com/xirang-grid/xirang/internal/domain"
)

const singleBestLinkPolicy = "single-best-healthy"

type pathCandidate struct {
	forward       domain.TopologyEdge
	reverse       domain.TopologyEdge
	bidirectional bool
	capacity      int64
	latency       int64
	health        string
}

// SelectParallelPaths preserves directed probe evidence and derives a
// conservative bidirectional path for scheduling. Declared capacity is the
// lower directional value, latency is the higher directional value, and probe
// paths with only one observed direction remain visible but have zero
// effective capacity. Non-probe topology facts retain their undirected legacy
// interpretation because they are not reachability measurements.
func SelectParallelPaths(edges []domain.TopologyEdge) []domain.NetworkPathSelection {
	groups := make(map[string][]domain.TopologyEdge)
	for _, edge := range edges {
		if edge.SourceID == "" || edge.TargetID == "" {
			continue
		}
		groups[pairKey(edge.SourceID, edge.TargetID)] = append(groups[pairKey(edge.SourceID, edge.TargetID)], edge)
	}
	selections := make([]domain.NetworkPathSelection, 0, len(groups))
	for key, observed := range groups {
		physical := make(map[string][]domain.TopologyEdge)
		for _, edge := range observed {
			if edge.Health == "critical" {
				continue
			}
			physical[physicalLinkKey(edge)] = append(physical[physicalLinkKey(edge)], edge)
		}
		candidates := make([]pathCandidate, 0, len(physical))
		for _, links := range physical {
			candidate, ok := combinePhysicalDirections(key, links)
			if ok {
				candidates = append(candidates, candidate)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool { return betterCandidate(candidates[i], candidates[j]) })
		primary := candidates[0]
		source, target := splitPairKey(key)
		selection := domain.NetworkPathSelection{
			SourceID: source, TargetID: target, PrimaryEdgeID: primary.forward.ID,
			PrimaryReverseEdgeID: primary.reverse.ID, EffectiveCapacityBits: primary.capacity,
			EffectiveLatencyMicros: primary.latency, Health: primary.health,
			Bidirectional: primary.bidirectional, Policy: singleBestLinkPolicy,
		}
		for _, fallback := range candidates[1:] {
			selection.FallbackEdgeIDs = append(selection.FallbackEdgeIDs, fallback.forward.ID)
			selection.FallbackReverseEdgeIDs = append(selection.FallbackReverseEdgeIDs, fallback.reverse.ID)
		}
		selections = append(selections, selection)
	}
	sort.Slice(selections, func(i, j int) bool {
		return pairKey(selections[i].SourceID, selections[i].TargetID) < pairKey(selections[j].SourceID, selections[j].TargetID)
	})
	return selections
}

func combinePhysicalDirections(key string, links []domain.TopologyEdge) (pathCandidate, bool) {
	addressPairs := make(map[string][]domain.TopologyEdge)
	for _, link := range links {
		if link.Type != "probe" {
			return combineDirections(key, links)
		}
		addressPairs[addressPairKey(link)] = append(addressPairs[addressPairKey(link)], link)
	}
	var best pathCandidate
	found := false
	for _, observations := range addressPairs {
		candidate, ok := combineDirections(key, observations)
		if ok && (!found || betterCandidate(candidate, best)) {
			best, found = candidate, true
		}
	}
	return best, found
}

func combineDirections(key string, links []domain.TopologyEdge) (pathCandidate, bool) {
	source, target := splitPairKey(key)
	var forward, reverse domain.TopologyEdge
	for _, link := range links {
		if link.Type != "probe" {
			if forward.ID == "" || betterLink(link, forward) {
				forward, reverse = link, link
			}
			continue
		}
		if link.SourceID == source && link.TargetID == target {
			if forward.ID == "" || betterLink(link, forward) {
				forward = link
			}
		} else if link.SourceID == target && link.TargetID == source {
			if reverse.ID == "" || betterLink(link, reverse) {
				reverse = link
			}
		}
	}
	if forward.ID == "" && reverse.ID == "" {
		return pathCandidate{}, false
	}
	bidirectional := forward.ID != "" && reverse.ID != ""
	if forward.ID == "" {
		forward = reverse
	}
	if forward.Type != "probe" {
		bidirectional = true
	}
	candidate := pathCandidate{forward: forward, reverse: reverse, bidirectional: bidirectional, health: "unverified", latency: math.MaxInt64}
	if bidirectional {
		candidate.capacity = min(forward.CapacityBits, reverse.CapacityBits)
		candidate.latency = max(topologyLatency(forward.LatencyMicros), topologyLatency(reverse.LatencyMicros))
		candidate.health = worseHealth(forward.Health, reverse.Health)
	}
	return candidate, true
}

func physicalLinkKey(link domain.TopologyEdge) string {
	if link.SourceInterface == "" {
		return link.ID
	}
	left := link.SourceID + "\x00" + link.SourceInterface
	right := link.TargetID + "\x00" + link.TargetInterface
	if left > right {
		left, right = right, left
	}
	return left + "\x00" + right
}

func addressPairKey(link domain.TopologyEdge) string {
	left := strings.Join([]string{link.SourceID, link.SourceInterface, link.SourceAddress}, "\x00")
	right := strings.Join([]string{link.TargetID, link.TargetInterface, link.TargetAddress}, "\x00")
	if left > right {
		left, right = right, left
	}
	return left + "\x00" + right
}

func pairKey(left, right string) string {
	if left > right {
		left, right = right, left
	}
	return left + "\x00" + right
}

func splitPairKey(key string) (string, string) {
	parts := strings.SplitN(key, "\x00", 2)
	return parts[0], parts[1]
}

func betterCandidate(left, right pathCandidate) bool {
	if left.bidirectional != right.bidirectional {
		return left.bidirectional
	}
	if !left.bidirectional {
		return betterLink(left.forward, right.forward)
	}
	if healthRank(left.health) != healthRank(right.health) {
		return healthRank(left.health) > healthRank(right.health)
	}
	if left.capacity != right.capacity {
		return left.capacity > right.capacity
	}
	if left.latency != right.latency {
		return left.latency < right.latency
	}
	return left.forward.ID < right.forward.ID
}

func betterLink(left, right domain.TopologyEdge) bool {
	if healthRank(left.Health) != healthRank(right.Health) {
		return healthRank(left.Health) > healthRank(right.Health)
	}
	if left.CapacityBits != right.CapacityBits {
		return left.CapacityBits > right.CapacityBits
	}
	if topologyLatency(left.LatencyMicros) != topologyLatency(right.LatencyMicros) {
		return topologyLatency(left.LatencyMicros) < topologyLatency(right.LatencyMicros)
	}
	return left.ID < right.ID
}

func topologyLatency(value int64) int64 {
	if value <= 0 {
		return math.MaxInt64
	}
	return value
}

func worseHealth(left, right string) string {
	if healthRank(left) <= healthRank(right) {
		return left
	}
	return right
}

func healthRank(health string) int {
	switch health {
	case "healthy":
		return 3
	case "degraded":
		return 2
	default:
		return 1
	}
}
