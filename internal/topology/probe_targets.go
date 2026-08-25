package topology

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
)

type ProbeNodeLister interface {
	ListNodes(context.Context) ([]domain.Node, error)
}

type ProbeInventoryReader interface {
	GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error)
}

type ProbeTargetProvider struct {
	nodes       ProbeNodeLister
	inventories ProbeInventoryReader
}

const maxProbeTargets = 4096

func NewProbeTargetProvider(nodes ProbeNodeLister, inventories ProbeInventoryReader) *ProbeTargetProvider {
	return &ProbeTargetProvider{nodes: nodes, inventories: inventories}
}

func (provider *ProbeTargetProvider) NetworkProbeTargets(ctx context.Context, excludeNodeID string) ([]*xirangv1.NetworkProbeTarget, error) {
	nodes, err := provider.nodes.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var targets []*xirangv1.NetworkProbeTarget
	for _, node := range nodes {
		if node.ID == excludeNodeID || node.Status == domain.NodeStatusRejected || node.Status == domain.NodeStatusPendingApproval {
			continue
		}
		inventory, err := provider.inventories.GetInventory(ctx, node.ID)
		if err != nil {
			continue
		}
		for _, networkInterface := range inventory.NetworkInterfaces {
			for _, rawAddress := range networkInterface.Addresses {
				address := strings.Split(strings.TrimSpace(rawAddress), "/")[0]
				parsed, err := netip.ParseAddr(address)
				if err != nil {
					continue
				}
				parsed = parsed.Unmap()
				if parsed.IsLoopback() || parsed.IsLinkLocalUnicast() || parsed.IsMulticast() || parsed.IsUnspecified() {
					continue
				}
				targets = append(targets, &xirangv1.NetworkProbeTarget{
					NodeId: node.ID, InterfaceName: networkInterface.Name, Address: parsed.String(),
					SpeedBitsPerSecond: networkInterface.SpeedBitsPerSecond,
				})
				if len(targets) > maxProbeTargets {
					return nil, errors.New("network probe target set exceeds 4096 addresses")
				}
			}
		}
	}
	sort.Slice(targets, func(left, right int) bool {
		return probeTargetKey(targets[left]) < probeTargetKey(targets[right])
	})
	return targets, nil
}

func probeTargetKey(target *xirangv1.NetworkProbeTarget) string {
	return strings.Join([]string{target.NodeId, target.InterfaceName, target.Address}, "\x00")
}
