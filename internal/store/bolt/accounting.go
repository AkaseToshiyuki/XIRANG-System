package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"go.etcd.io/bbolt"
)

func (store *Store) ApplyAssignmentUsage(ctx context.Context, snapshot domain.AssignmentUsageSnapshot) (domain.Job, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, false, err
	}
	var result domain.Job
	changed := false
	err := store.database.Update(func(tx *bbolt.Tx) error {
		jobID := tx.Bucket(jobAssignmentsBucket).Get([]byte(snapshot.AssignmentID))
		if jobID == nil {
			// A final usage sample can race the terminal state report on the
			// multiplexed control stream. Terminal jobs retain AssignmentID, so
			// locate it without reopening scheduler reservations.
			err := tx.Bucket(jobsBucket).ForEach(func(key, encoded []byte) error {
				var candidate domain.Job
				if err := json.Unmarshal(encoded, &candidate); err != nil {
					return err
				}
				matched := candidate.AssignmentID == snapshot.AssignmentID
				if candidate.Allocation != nil {
					_, matched = candidate.Allocation.MemberByAssignment(snapshot.AssignmentID)
				}
				if matched {
					jobID = append([]byte(nil), key...)
				}
				return nil
			})
			if err != nil || jobID == nil {
				return err
			}
		}
		job, err := jobFromBucket(tx, string(jobID))
		if err != nil {
			return err
		}
		previous := job.UsageCheckpoint
		var member *domain.AllocationMember
		if job.Allocation != nil {
			var ok bool
			member, ok = job.Allocation.MemberByAssignment(snapshot.AssignmentID)
			if !ok || member.NodeID != snapshot.NodeID || job.Attempt != snapshot.Attempt {
				return nil
			}
			previous = member.UsageCheckpoint
		} else if job.AssignmentID != snapshot.AssignmentID || job.NodeID != snapshot.NodeID || job.Attempt != snapshot.Attempt {
			return nil
		}
		if previous.ObservedAt != nil && !snapshot.ObservedAt.After(*previous.ObservedAt) {
			return nil
		}
		// Cumulative counters must never move backwards within one attempt.
		if snapshot.CPUUsageMicros < previous.CPUUsageMicros ||
			snapshot.MemoryByteSeconds < previous.MemoryByteSeconds ||
			snapshot.GPUDeviceSeconds < previous.GPUDeviceSeconds ||
			snapshot.WallTimeMillis < previous.WallTimeMillis {
			return errors.New("assignment usage counters moved backwards")
		}
		delta := domain.DailyUsage{
			Date: snapshot.ObservedAt.UTC().Format("2006-01-02"), OwnerID: job.OwnerID, OwnerName: job.OwnerName,
			CPUUsageMicros:     snapshot.CPUUsageMicros - previous.CPUUsageMicros,
			MemoryByteSeconds:  snapshot.MemoryByteSeconds - previous.MemoryByteSeconds,
			GPUDeviceSeconds:   snapshot.GPUDeviceSeconds - previous.GPUDeviceSeconds,
			WallTimeMillis:     snapshot.WallTimeMillis - previous.WallTimeMillis,
			PeakWorkspaceBytes: snapshot.WorkspaceBytes,
		}
		if previous.ObservedAt != nil {
			seconds := uint64(snapshot.ObservedAt.Sub(*previous.ObservedAt) / time.Second)
			delta.WorkspaceByteSeconds = saturatingMultiply(previous.WorkspaceBytes, seconds)
		}
		if err := addDailyUsage(tx, delta); err != nil {
			return err
		}
		observedAt := snapshot.ObservedAt.UTC()
		checkpoint := domain.JobUsage{
			CPUUsageMicros: snapshot.CPUUsageMicros, MemoryByteSeconds: snapshot.MemoryByteSeconds,
			GPUDeviceSeconds: snapshot.GPUDeviceSeconds, WallTimeMillis: snapshot.WallTimeMillis,
			WorkspaceBytes: snapshot.WorkspaceBytes, PeakWorkspaceBytes: previous.PeakWorkspaceBytes,
			FilesystemAvailableBytes: snapshot.FilesystemAvailableBytes, FilesystemTotalBytes: snapshot.FilesystemTotalBytes,
			ObservedAt: &observedAt,
		}
		if member != nil {
			member.UsageCheckpoint = checkpoint
			member.Usage = accumulateJobUsage(member.Usage, snapshot, delta, observedAt)
		} else {
			job.UsageCheckpoint = checkpoint
		}
		total := job.Usage
		total.CPUUsageMicros = saturatingAdd(total.CPUUsageMicros, delta.CPUUsageMicros)
		total.MemoryByteSeconds = saturatingAdd(total.MemoryByteSeconds, delta.MemoryByteSeconds)
		total.GPUDeviceSeconds = saturatingAdd(total.GPUDeviceSeconds, delta.GPUDeviceSeconds)
		total.WallTimeMillis = saturatingAdd(total.WallTimeMillis, delta.WallTimeMillis)
		if member != nil {
			if total.WorkspaceBytes >= previous.WorkspaceBytes {
				total.WorkspaceBytes -= previous.WorkspaceBytes
			} else {
				total.WorkspaceBytes = 0
			}
			total.WorkspaceBytes = saturatingAdd(total.WorkspaceBytes, snapshot.WorkspaceBytes)
			if total.FilesystemAvailableBytes == 0 || (snapshot.FilesystemAvailableBytes > 0 && snapshot.FilesystemAvailableBytes < total.FilesystemAvailableBytes) {
				total.FilesystemAvailableBytes = snapshot.FilesystemAvailableBytes
			}
			if snapshot.FilesystemTotalBytes > total.FilesystemTotalBytes {
				total.FilesystemTotalBytes = snapshot.FilesystemTotalBytes
			}
		} else {
			total.WorkspaceBytes = snapshot.WorkspaceBytes
			total.FilesystemAvailableBytes, total.FilesystemTotalBytes = snapshot.FilesystemAvailableBytes, snapshot.FilesystemTotalBytes
		}
		total.ObservedAt = &observedAt
		if snapshot.WorkspaceBytes > total.PeakWorkspaceBytes {
			total.PeakWorkspaceBytes = snapshot.WorkspaceBytes
		}
		job.Usage = total
		job.UpdatedAt = snapshot.ReceivedAt.UTC()
		if err := putJob(tx, job); err != nil {
			return err
		}
		result, changed = job, true
		return nil
	})
	return result, changed, err
}

func accumulateJobUsage(total domain.JobUsage, snapshot domain.AssignmentUsageSnapshot, delta domain.DailyUsage, observedAt time.Time) domain.JobUsage {
	total.CPUUsageMicros = saturatingAdd(total.CPUUsageMicros, delta.CPUUsageMicros)
	total.MemoryByteSeconds = saturatingAdd(total.MemoryByteSeconds, delta.MemoryByteSeconds)
	total.GPUDeviceSeconds = saturatingAdd(total.GPUDeviceSeconds, delta.GPUDeviceSeconds)
	total.WallTimeMillis = saturatingAdd(total.WallTimeMillis, delta.WallTimeMillis)
	total.WorkspaceBytes = snapshot.WorkspaceBytes
	total.FilesystemAvailableBytes, total.FilesystemTotalBytes = snapshot.FilesystemAvailableBytes, snapshot.FilesystemTotalBytes
	total.ObservedAt = &observedAt
	if snapshot.WorkspaceBytes > total.PeakWorkspaceBytes {
		total.PeakWorkspaceBytes = snapshot.WorkspaceBytes
	}
	return total
}

func addDailyUsage(tx *bbolt.Tx, delta domain.DailyUsage) error {
	if delta.OwnerID == "" {
		return nil
	}
	key := []byte(delta.Date + "\x00" + delta.OwnerID)
	bucket := tx.Bucket(usageDailyBucket)
	var current domain.DailyUsage
	if encoded := bucket.Get(key); encoded != nil {
		if err := json.Unmarshal(encoded, &current); err != nil {
			return err
		}
	} else {
		current.Date, current.OwnerID, current.OwnerName = delta.Date, delta.OwnerID, delta.OwnerName
	}
	current.CPUUsageMicros = saturatingAdd(current.CPUUsageMicros, delta.CPUUsageMicros)
	current.MemoryByteSeconds = saturatingAdd(current.MemoryByteSeconds, delta.MemoryByteSeconds)
	current.GPUDeviceSeconds = saturatingAdd(current.GPUDeviceSeconds, delta.GPUDeviceSeconds)
	current.WallTimeMillis = saturatingAdd(current.WallTimeMillis, delta.WallTimeMillis)
	current.WorkspaceByteSeconds = saturatingAdd(current.WorkspaceByteSeconds, delta.WorkspaceByteSeconds)
	if delta.PeakWorkspaceBytes > current.PeakWorkspaceBytes {
		current.PeakWorkspaceBytes = delta.PeakWorkspaceBytes
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return bucket.Put(key, encoded)
}

func saturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func saturatingMultiply(left, right uint64) uint64 {
	if left != 0 && right > math.MaxUint64/left {
		return math.MaxUint64
	}
	return left * right
}

func (store *Store) ListDailyUsage(ctx context.Context, from, to, ownerID string) ([]domain.DailyUsage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := []domain.DailyUsage{}
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(usageDailyBucket).ForEach(func(_, encoded []byte) error {
			var usage domain.DailyUsage
			if err := json.Unmarshal(encoded, &usage); err != nil {
				return err
			}
			if usage.Date < from || usage.Date > to || (ownerID != "" && usage.OwnerID != ownerID) {
				return nil
			}
			result = append(result, usage)
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool {
		if result[i].Date != result[j].Date {
			return result[i].Date < result[j].Date
		}
		return result[i].OwnerID < result[j].OwnerID
	})
	return result, err
}

func (store *Store) PutBillingRate(ctx context.Context, rate domain.BillingRate, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(billingRatesBucket)
		if bucket.Get([]byte(rate.ID)) != nil {
			return errors.New("billing rate already exists")
		}
		encoded, err := json.Marshal(rate)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(rate.ID), encoded); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}

func (store *Store) ListBillingRates(ctx context.Context) ([]domain.BillingRate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rates := []domain.BillingRate{}
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(billingRatesBucket).ForEach(func(_, encoded []byte) error {
			var rate domain.BillingRate
			if err := json.Unmarshal(encoded, &rate); err != nil {
				return err
			}
			if strings.TrimSpace(rate.Currency) != "" {
				rates = append(rates, rate)
			}
			return nil
		})
	})
	sort.Slice(rates, func(i, j int) bool { return rates[i].EffectiveAt.Before(rates[j].EffectiveAt) })
	return rates, err
}
