package networkhealth

import (
	"sort"

	"github.com/xirang-grid/xirang/internal/domain"
)

const singleBestLinkPolicy = "single-best-healthy"

// SelectParallelPaths chooses one effective link for every endpoint pair and
// retains the other healthy links in deterministic failover order. Critical
// links are excluded; disappearing links naturally leave the next snapshot.
func SelectParallelPaths(edges []domain.TopologyEdge) []domain.NetworkPathSelection {
	groups := make(map[string][]domain.TopologyEdge)
	for _, edge := range edges {
		if edge.SourceID == "" || edge.TargetID == "" {
			continue
		}
		source, target := edge.SourceID, edge.TargetID
		if source > target {
			source, target = target, source
		}
		groups[source+"\x00"+target] = append(groups[source+"\x00"+target], edge)
	}
	selections := make([]domain.NetworkPathSelection, 0, len(groups))
	for key, observedLinks := range groups {
		physicalLinks := make(map[string]domain.TopologyEdge)
		for _, link := range observedLinks {
			if link.Health == "critical" {
				continue
			}
			physicalKey := physicalLinkKey(link)
			if link.SourceInterface == "" {
				physicalKey = link.ID
			}
			if current, exists := physicalLinks[physicalKey]; !exists || betterLink(link, current) {
				physicalLinks[physicalKey] = link
			}
		}
		links := make([]domain.TopologyEdge, 0, len(physicalLinks))
		for _, link := range physicalLinks {
			links = append(links, link)
		}
		if len(links) == 0 {
			continue
		}
		sort.Slice(links, func(i, j int) bool { return betterLink(links[i], links[j]) })
		separator := 0
		for index := range key {
			if key[index] == 0 {
				separator = index
				break
			}
		}
		selection := domain.NetworkPathSelection{
			SourceID: key[:separator], TargetID: key[separator+1:], PrimaryEdgeID: links[0].ID,
			EffectiveCapacityBits: links[0].CapacityBits, Policy: singleBestLinkPolicy,
		}
		for _, link := range links[1:] {
			selection.FallbackEdgeIDs = append(selection.FallbackEdgeIDs, link.ID)
		}
		selections = append(selections, selection)
	}
	sort.Slice(selections, func(i, j int) bool {
		return selections[i].SourceID+"\x00"+selections[i].TargetID < selections[j].SourceID+"\x00"+selections[j].TargetID
	})
	return selections
}

func physicalLinkKey(link domain.TopologyEdge) string {
	if link.SourceInterface == "" {
		return link.ID
	}
	// Multiple stable/privacy addresses on the same interface do not provide
	// independent capacity. Preserve every address-pair probe as evidence, but
	// collapse selection and fallback ordering to one physical interface pair.
	source := link.SourceID + "\x00" + link.SourceInterface
	target := link.TargetID + "\x00" + link.TargetInterface
	if source > target {
		source, target = target, source
	}
	return source + "\x00" + target
}

func betterLink(left, right domain.TopologyEdge) bool {
	if healthRank(left.Health) != healthRank(right.Health) {
		return healthRank(left.Health) > healthRank(right.Health)
	}
	if left.CapacityBits != right.CapacityBits {
		return left.CapacityBits > right.CapacityBits
	}
	leftLatency, rightLatency := left.LatencyMicros, right.LatencyMicros
	if leftLatency <= 0 {
		leftLatency = int64(^uint64(0) >> 1)
	}
	if rightLatency <= 0 {
		rightLatency = int64(^uint64(0) >> 1)
	}
	if leftLatency != rightLatency {
		return leftLatency < rightLatency
	}
	return left.ID < right.ID
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
