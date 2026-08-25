// Package flow validates and persists bounded, payload-free eBPF flow reports.
package flow

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
)

var ErrInvalidReport = errors.New("invalid flow report")

type Repository interface {
	Append(context.Context, domain.NodeFlowObservation) error
	List(context.Context, string, int) ([]domain.NodeFlowObservation, error)
	ListLatest(context.Context) ([]domain.NodeFlowObservation, error)
	Close() error
}

type Service struct {
	repository Repository
	now        func() time.Time
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository, now: time.Now}
}

func (service *Service) Report(ctx context.Context, nodeID, messageID string, observedAt time.Time, input *xirangv1.FlowObservation) error {
	now := service.now()
	if input == nil || !validText(nodeID, 256) || !validText(messageID, 256) || len(input.Flows) > 4096 || input.MapEntries > 1<<20 ||
		input.Interval == nil || input.Interval.CheckValid() != nil || input.Interval.AsDuration() < time.Second || input.Interval.AsDuration() > 5*time.Minute ||
		observedAt.IsZero() || observedAt.Before(now.Add(-24*time.Hour)) || observedAt.After(now.Add(5*time.Minute)) {
		return ErrInvalidReport
	}
	observation := domain.NodeFlowObservation{
		MessageID: messageID, NodeID: nodeID, Truncated: input.Truncated, MapEntries: input.MapEntries,
		Interval: input.Interval.AsDuration(), ObservedAt: observedAt.UTC(), ReceivedAt: now.UTC(),
	}
	for _, item := range input.Flows {
		converted, ok := validatedFlow(item)
		if !ok {
			return ErrInvalidReport
		}
		observation.Flows = append(observation.Flows, converted)
	}
	sort.Slice(observation.Flows, func(i, j int) bool { return observation.Flows[i].Bytes > observation.Flows[j].Bytes })
	return service.repository.Append(ctx, observation)
}

func (service *Service) List(ctx context.Context, nodeID string, limit int) ([]domain.NodeFlowObservation, error) {
	if !validText(nodeID, 256) || limit < 1 || limit > 1000 {
		return nil, ErrInvalidReport
	}
	return service.repository.List(ctx, nodeID, limit)
}

func (service *Service) ListLatest(ctx context.Context) ([]domain.NodeFlowObservation, error) {
	return service.repository.ListLatest(ctx)
}

func validatedFlow(item *xirangv1.NetworkFlow) (domain.NetworkFlow, bool) {
	if item == nil || (item.Family != "ipv4" && item.Family != "ipv6") || (item.Protocol != "tcp" && item.Protocol != "udp") ||
		(item.Direction != "ingress" && item.Direction != "egress") || item.SourcePort > 65535 || item.DestinationPort > 65535 ||
		item.Packets == 0 || item.Bytes < item.Packets {
		return domain.NetworkFlow{}, false
	}
	source, sourceErr := netip.ParseAddr(item.SourceAddress)
	destination, destinationErr := netip.ParseAddr(item.DestinationAddress)
	if sourceErr != nil || destinationErr != nil || source.Is4() != destination.Is4() || (item.Family == "ipv4") != source.Is4() {
		return domain.NetworkFlow{}, false
	}
	return domain.NetworkFlow{
		Family: item.Family, Protocol: item.Protocol,
		SourceAddress: source.String(), SourcePort: uint16(item.SourcePort),
		DestinationAddress: destination.String(), DestinationPort: uint16(item.DestinationPort),
		Direction: item.Direction, Packets: item.Packets, Bytes: item.Bytes,
	}, true
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsRune(value, '\x00')
}
