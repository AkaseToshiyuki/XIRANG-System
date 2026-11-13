package clusterops

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func decodeSharedCapacity(output []byte, at time.Time) (capacityObservation, error) {
	var wire struct {
		UsedBytes, AvailableBytes, TotalBytes   uint64
		QuotaStatus                             string
		QuotaEnforcedBytes, QuotaAllocatedBytes uint64
		QuotaError                              string
	}
	if err := json.Unmarshal(output, &wire); err != nil || wire.TotalBytes == 0 {
		return capacityObservation{}, fmt.Errorf("invalid shared capacity response")
	}
	switch wire.QuotaStatus {
	case "": // older source Agents have no quota observation
		wire.QuotaStatus = "unknown"
	case "available", "unlimited", "enforced", "unsupported", "unknown":
	default:
		return capacityObservation{}, fmt.Errorf("invalid shared quota observation")
	}
	if wire.QuotaStatus == "enforced" && wire.QuotaEnforcedBytes == 0 {
		return capacityObservation{}, fmt.Errorf("missing enforced capacity limit")
	}
	return capacityObservation{usedBytes: wire.UsedBytes, availableBytes: wire.AvailableBytes, totalBytes: wire.TotalBytes,
		observedAt: at, quotaStatus: wire.QuotaStatus, quotaEnforcedBytes: wire.QuotaEnforcedBytes,
		quotaAllocatedBytes: wire.QuotaAllocatedBytes, quotaError: wire.QuotaError}, nil
}

// UpdateQuota is an explicit administrative action. Reconciliation never turns
// a persisted desired limit into an unverified enforcement claim or silently
// changes filesystem-wide quota settings. The host validates current free space
// and reads the kernel limit back before acknowledging the operation.
func (service *Service) UpdateQuota(ctx context.Context, id string, limit uint64) (domain.SharedSpace, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if limit > 1<<60 || limit%1024 != 0 {
		return domain.SharedSpace{}, fmt.Errorf("%w: capacity limit must be zero or a whole number of KiB", ErrInvalid)
	}
	space, err := service.repository.GetSharedSpace(ctx, id)
	if err != nil {
		return domain.SharedSpace{}, err
	}
	source, err := service.nodes.GetNode(ctx, space.SourceNodeID)
	if err != nil || nodeUnavailable(source) || nodeNotOnline(source) {
		return domain.SharedSpace{}, fmt.Errorf("%w: shared source node is unavailable", ErrInvalid)
	}
	// Do not rely on a 30-second cached or client-supplied capacity value.
	// quota-set checks the underlying filesystem immediately before applying.
	_, err = service.runner.Run(ctx, source, "quota-set", space.ID, space.SourcePath, strconv.FormatUint(limit, 10))
	service.invalidateCapacity(id)
	if err != nil {
		return domain.SharedSpace{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	space.QuotaBytes, space.UpdatedAt = limit, service.now().UTC()
	if err := service.repository.PutSharedSpace(ctx, space); err != nil {
		return domain.SharedSpace{}, fmt.Errorf("kernel quota updated, but saving its configuration failed: %w", err)
	}
	output, err := service.runner.Run(ctx, source, "stats", space.ID, space.SourcePath)
	if err != nil {
		return domain.SharedSpace{}, fmt.Errorf("kernel quota updated, but its capacity observation is unavailable: %w", err)
	}
	observation, err := decodeSharedCapacity(output, service.now().UTC())
	if err != nil || observation.quotaEnforcedBytes != limit || limit != 0 && observation.quotaStatus != "enforced" {
		return domain.SharedSpace{}, fmt.Errorf("%w: quota update was acknowledged but its independent observation did not confirm enforcement; refresh the shared space", ErrInvalid)
	}
	service.storeCapacity(id, observation)
	applyCapacity(&space, observation)
	space.FileOwners = nil
	return space, nil
}
