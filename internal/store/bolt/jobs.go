package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/scheduler"
	"go.etcd.io/bbolt"
)

func (store *Store) CreateJob(ctx context.Context, job domain.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(jobsBucket)
		if bucket.Get([]byte(job.ID)) != nil {
			return scheduler.ErrConflict
		}
		return bucket.Put([]byte(job.ID), encoded)
	})
}

func (store *Store) GetJob(ctx context.Context, id string) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var job domain.Job
	err := store.database.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(jobsBucket).Get([]byte(id))
		if encoded == nil {
			return scheduler.ErrNotFound
		}
		return json.Unmarshal(encoded, &job)
	})
	return job, err
}

func (store *Store) ListJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jobs := make([]domain.Job, 0)
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(jobsBucket).ForEach(func(_, encoded []byte) error {
			var job domain.Job
			if err := json.Unmarshal(encoded, &job); err != nil {
				return err
			}
			jobs = append(jobs, job)
			return nil
		})
	})
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Status == domain.JobStatusQueued && jobs[j].Status == domain.JobStatusQueued && jobs[i].Priority != jobs[j].Priority {
			return jobs[i].Priority > jobs[j].Priority
		}
		if jobs[i].Status == domain.JobStatusQueued && jobs[j].Status != domain.JobStatusQueued {
			return true
		}
		if jobs[i].Status != domain.JobStatusQueued && jobs[j].Status == domain.JobStatusQueued {
			return false
		}
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, err
}

func (store *Store) UpdateGroupJobStorageQuota(ctx context.Context, groupID string, quotaBytes int64, now time.Time) ([]domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	updated := []domain.Job{}
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(jobsBucket)
		return bucket.ForEach(func(key, encoded []byte) error {
			var job domain.Job
			if err := json.Unmarshal(encoded, &job); err != nil {
				return err
			}
			if !jobUsesPolicyGroup(job, groupID) || job.Status.Terminal() {
				return nil
			}
			job.StorageQuotaBytes, job.UpdatedAt = quotaBytes, now.UTC()
			replacement, err := json.Marshal(job)
			if err != nil {
				return err
			}
			if err := bucket.Put(key, replacement); err != nil {
				return err
			}
			updated = append(updated, job)
			return nil
		})
	})
	return updated, err
}

func (store *Store) UpdateUserJobStorageQuota(ctx context.Context, ownerID string, quotaBytes int64, now time.Time) ([]domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	updated := []domain.Job{}
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(jobsBucket)
		return bucket.ForEach(func(key, encoded []byte) error {
			var job domain.Job
			if err := json.Unmarshal(encoded, &job); err != nil {
				return err
			}
			if job.OwnerID != ownerID || job.Status.Terminal() {
				return nil
			}
			job.StorageQuotaBytes, job.UpdatedAt = quotaBytes, now.UTC()
			replacement, err := json.Marshal(job)
			if err != nil {
				return err
			}
			if err := bucket.Put(key, replacement); err != nil {
				return err
			}
			updated = append(updated, job)
			return nil
		})
	})
	return updated, err
}

func (store *Store) SetGroupJobsSuspended(ctx context.Context, groupID string, suspended bool, reason string, now time.Time) ([]domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	updated := []domain.Job{}
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(jobsBucket)
		return bucket.ForEach(func(key, encoded []byte) error {
			var job domain.Job
			if err := json.Unmarshal(encoded, &job); err != nil {
				return err
			}
			if !jobUsesPolicyGroup(job, groupID) {
				return nil
			}
			if job.AssignmentID != "" || (job.Status != domain.JobStatusQueued && (job.Status != domain.JobStatusSuspended || job.BlockReason == nil || job.BlockReason.Code != "group_disabled")) {
				return nil
			}
			groups := append([]string(nil), job.SuspendedPolicyGroupIDs...)
			if len(groups) == 0 && job.BlockReason != nil && job.BlockReason.Details["groupId"] != "" {
				groups = append(groups, job.BlockReason.Details["groupId"])
			}
			if suspended {
				if !containsGroup(groups, groupID) {
					groups = append(groups, groupID)
				}
				sort.Strings(groups)
				job.Status, job.Reason, job.SuspendedPolicyGroupIDs, job.UpdatedAt = domain.JobStatusSuspended, reason, groups, now.UTC()
				job.BlockReason = groupDisabledBlockReason(reason, groups, now)
			} else {
				remaining := removeGroup(groups, groupID)
				if len(remaining) == len(groups) {
					return nil
				}
				job.SuspendedPolicyGroupIDs, job.UpdatedAt = remaining, now.UTC()
				if len(remaining) == 0 {
					job.Status, job.Reason, job.BlockReason = domain.JobStatusQueued, "", nil
				} else {
					job.Status, job.Reason = domain.JobStatusSuspended, reason
					job.BlockReason = groupDisabledBlockReason(reason, remaining, now)
				}
			}
			replacement, err := json.Marshal(job)
			if err != nil {
				return err
			}
			if err := bucket.Put(key, replacement); err != nil {
				return err
			}
			updated = append(updated, job)
			return nil
		})
	})
	return updated, err
}

func groupDisabledBlockReason(reason string, groups []string, now time.Time) *domain.JobBlockReason {
	details := map[string]string{"groupIds": strings.Join(groups, ",")}
	if len(groups) > 0 {
		details["groupId"] = groups[0]
	}
	return &domain.JobBlockReason{Code: "group_disabled", Category: "policy", Message: reason, Details: details, ObservedAt: now.UTC()}
}

func containsGroup(groups []string, groupID string) bool {
	for _, id := range groups {
		if id == groupID {
			return true
		}
	}
	return false
}

func removeGroup(groups []string, groupID string) []string {
	result := make([]string, 0, len(groups))
	for _, id := range groups {
		if id != groupID {
			result = append(result, id)
		}
	}
	return result
}

func jobUsesPolicyGroup(job domain.Job, groupID string) bool {
	if job.GroupID == groupID {
		return true
	}
	for _, id := range job.PolicyGroupIDs {
		if id == groupID {
			return true
		}
	}
	return false
}

func (store *Store) FailQueuedJob(ctx context.Context, jobID, reason string, now time.Time) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var result domain.Job
	err := store.database.Update(func(tx *bbolt.Tx) error {
		job, err := jobFromBucket(tx, jobID)
		if err != nil {
			return err
		}
		if job.Status != domain.JobStatusQueued || job.AssignmentID != "" {
			return scheduler.ErrConflict
		}
		finished := now.UTC()
		job.Status, job.Reason, job.UpdatedAt, job.FinishedAt = domain.JobStatusFailed, reason, finished, &finished
		if err := putJob(tx, job); err != nil {
			return err
		}
		result = job
		return nil
	})
	return result, err
}

// UpdateQueuedJobBlock persists scheduler diagnostics without changing queue
// order or resource reservations. Repeated identical observations are a no-op
// so the one-second scheduling reconciliation does not create write churn.
func (store *Store) UpdateQueuedJobBlock(ctx context.Context, jobID string, block *domain.JobBlockReason, now time.Time) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var result domain.Job
	err := store.database.Update(func(tx *bbolt.Tx) error {
		job, err := jobFromBucket(tx, jobID)
		if err != nil {
			return err
		}
		if job.Status != domain.JobStatusQueued || job.AssignmentID != "" || job.Allocation != nil {
			return scheduler.ErrConflict
		}
		if equalJobBlockReason(job.BlockReason, block) {
			result = job
			return nil
		}
		if block == nil {
			job.BlockReason = nil
			job.Reason = ""
		} else {
			copy := *block
			copy.ObservedAt = now.UTC()
			copy.Details = cloneStringMap(block.Details)
			job.BlockReason = &copy
			job.Reason = copy.Message
		}
		job.UpdatedAt = now.UTC()
		if err := putJob(tx, job); err != nil {
			return err
		}
		result = job
		return nil
	})
	return result, err
}

func equalJobBlockReason(left, right *domain.JobBlockReason) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if left.Code != right.Code || left.Category != right.Category || left.Message != right.Message || len(left.Details) != len(right.Details) {
		return false
	}
	for key, value := range left.Details {
		if right.Details[key] != value {
			return false
		}
	}
	return true
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func (store *Store) AssignJob(ctx context.Context, jobID, nodeID, assignmentID string, now time.Time) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var assigned domain.Job
	err := store.database.Update(func(tx *bbolt.Tx) error {
		job, err := jobFromBucket(tx, jobID)
		if err != nil {
			return err
		}
		if job.Status != domain.JobStatusQueued || job.AssignmentID != "" ||
			int64(job.Attempt) >= int64(job.MaxAttempts)+int64(job.PreemptionCount) {
			return scheduler.ErrConflict
		}
		node, err := nodeFromBucket(tx, nodeID)
		if err != nil {
			return err
		}
		if !scheduler.NodeFitsJob(node, job) {
			return scheduler.ErrNoCapacity
		}
		node.Allocated = node.Allocated.Add(job.Task.Resources)
		node.UpdatedAt = now.UTC()
		node.Revision++
		job.Attempt++
		job.AssignmentID, job.NodeID = assignmentID, nodeID
		job.Status, job.UpdatedAt = domain.JobStatusAssigned, now.UTC()
		job.Reason, job.BlockReason, job.ExitCode, job.StartedAt, job.FinishedAt = "", nil, 0, nil, nil
		job.PreemptionNodeID, job.PreemptionVictims = "", nil
		job.StdoutTail, job.StderrTail, job.LogsTruncated, job.LogsObservedAt = "", "", false, nil
		job.UsageCheckpoint = domain.JobUsage{}
		if err := putNode(tx, node); err != nil {
			return err
		}
		if err := putJob(tx, job); err != nil {
			return err
		}
		if err := tx.Bucket(jobAssignmentsBucket).Put([]byte(assignmentID), []byte(jobID)); err != nil {
			return err
		}
		assigned = job
		return nil
	})
	return assigned, err
}

// AssignJobAllocation performs the R3 gang reservation in one Bolt
// transaction. No node allocation is visible unless every member still fits.
func (store *Store) AssignJobAllocation(ctx context.Context, jobID string, allocation domain.Allocation, now time.Time) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var assigned domain.Job
	err := store.database.Update(func(tx *bbolt.Tx) error {
		job, err := jobFromBucket(tx, jobID)
		if err != nil {
			return err
		}
		parallel := job.Task.Parallel.Effective()
		if job.Status != domain.JobStatusQueued || job.AssignmentID != "" || job.Allocation != nil ||
			int64(job.Attempt) >= int64(job.MaxAttempts)+int64(job.PreemptionCount) ||
			allocation.ID == "" || len(allocation.Members) != int(parallel.Nodes) || len(allocation.Members) < 2 {
			return scheduler.ErrConflict
		}
		seenNodes, seenAssignments := make(map[string]bool), make(map[string]bool)
		nodes := make([]domain.Node, 0, len(allocation.Members))
		for _, member := range allocation.Members {
			if member.NodeID == "" || member.AssignmentID == "" || seenNodes[member.NodeID] || seenAssignments[member.AssignmentID] {
				return scheduler.ErrConflict
			}
			seenNodes[member.NodeID], seenAssignments[member.AssignmentID] = true, true
			node, err := nodeFromBucket(tx, member.NodeID)
			if err != nil {
				return err
			}
			if !scheduler.NodeFitsJob(node, job) {
				return scheduler.ErrNoCapacity
			}
			nodes = append(nodes, node)
		}
		updatedAt := now.UTC()
		for index, node := range nodes {
			node.Allocated = node.Allocated.Add(job.Task.Resources)
			node.UpdatedAt, node.Revision = updatedAt, node.Revision+1
			if err := putNode(tx, node); err != nil {
				return err
			}
			if err := tx.Bucket(jobAssignmentsBucket).Put([]byte(allocation.Members[index].AssignmentID), []byte(jobID)); err != nil {
				return err
			}
		}
		job.Attempt++
		allocation.Attempt, allocation.State = job.Attempt, domain.AllocationStatePreparing
		allocation.CreatedAt, allocation.UpdatedAt = updatedAt, updatedAt
		for index := range allocation.Members {
			allocation.Members[index].State = domain.AssignmentStateAccepted
			allocation.Members[index].UsageCheckpoint = domain.JobUsage{}
		}
		job.Allocation = &allocation
		job.AssignmentID, job.NodeID = allocation.Members[0].AssignmentID, allocation.Members[0].NodeID
		job.Status, job.UpdatedAt = domain.JobStatusAssigned, updatedAt
		job.Reason, job.BlockReason, job.ExitCode, job.StartedAt, job.FinishedAt = "preparing gang allocation", nil, 0, nil, nil
		job.StdoutTail, job.StderrTail, job.LogsTruncated, job.LogsObservedAt = "", "", false, nil
		job.UsageCheckpoint = domain.JobUsage{}
		if err := putJob(tx, job); err != nil {
			return err
		}
		assigned = job
		return nil
	})
	return assigned, err
}

func (store *Store) RollbackJobAllocation(ctx context.Context, jobID, allocationID, reason string, now time.Time) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var updated domain.Job
	err := store.database.Update(func(tx *bbolt.Tx) error {
		job, err := jobFromBucket(tx, jobID)
		if err != nil {
			return err
		}
		if job.Status != domain.JobStatusAssigned || job.Allocation == nil || job.Allocation.ID != allocationID {
			return scheduler.ErrConflict
		}
		if err := releaseAllocationResources(tx, job, now); err != nil {
			return err
		}
		for _, member := range job.Allocation.Members {
			if err := tx.Bucket(jobAssignmentsBucket).Delete([]byte(member.AssignmentID)); err != nil {
				return err
			}
		}
		job.Status, job.AssignmentID, job.NodeID, job.Allocation = domain.JobStatusQueued, "", "", nil
		job.Reason, job.UpdatedAt = reason, now.UTC()
		if job.Attempt > 0 {
			job.Attempt--
		}
		if err := putJob(tx, job); err != nil {
			return err
		}
		updated = job
		return nil
	})
	return updated, err
}

func (store *Store) RollbackAssignment(ctx context.Context, jobID, assignmentID, reason string, now time.Time) (domain.Job, error) {
	return store.finishUndeliveredAssignment(ctx, jobID, assignmentID, reason, now, false)
}

func (store *Store) RejectAssignment(ctx context.Context, jobID, assignmentID, reason string, now time.Time) (domain.Job, error) {
	return store.finishUndeliveredAssignment(ctx, jobID, assignmentID, reason, now, true)
}

func (store *Store) RequestJobCancellation(ctx context.Context, jobID, reason string, grace time.Duration, now time.Time) (domain.Job, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, false, err
	}
	var updated domain.Job
	dispatch := false
	err := store.database.Update(func(tx *bbolt.Tx) error {
		job, err := jobFromBucket(tx, jobID)
		if err != nil {
			return err
		}
		switch job.Status {
		case domain.JobStatusQueued:
			finished := now.UTC()
			job.Status, job.Reason, job.UpdatedAt, job.FinishedAt = domain.JobStatusCanceled, reason, finished, &finished
		case domain.JobStatusSuspended:
			if job.AssignmentID == "" {
				finished := now.UTC()
				job.Status, job.Reason, job.BlockReason, job.SuspendedPolicyGroupIDs, job.UpdatedAt, job.FinishedAt = domain.JobStatusCanceled, reason, nil, nil, finished, &finished
				break
			}
			fallthrough
		case domain.JobStatusAssigned, domain.JobStatusRunning, domain.JobStatusOrphaned:
			job.Status, job.Reason, job.CancelGracePeriod, job.UpdatedAt = domain.JobStatusCanceling, reason, grace, now.UTC()
			if job.Allocation != nil {
				job.Allocation.State, job.Allocation.UpdatedAt = domain.AllocationStateCanceling, now.UTC()
			}
			dispatch = true
		case domain.JobStatusCanceling:
			updated = job
			return nil
		default:
			return scheduler.ErrConflict
		}
		if err := putJob(tx, job); err != nil {
			return err
		}
		updated = job
		return nil
	})
	return updated, dispatch, err
}

func (store *Store) RequestJobPreemption(
	ctx context.Context,
	preemptorID, nodeID string,
	victimIDs []string,
	reason string,
	grace time.Duration,
	now time.Time,
) ([]domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(victimIDs) == 0 {
		return nil, scheduler.ErrConflict
	}
	requested := make([]domain.Job, 0, len(victimIDs))
	err := store.database.Update(func(tx *bbolt.Tx) error {
		preemptor, err := jobFromBucket(tx, preemptorID)
		if err != nil {
			return err
		}
		if preemptor.Status != domain.JobStatusQueued || len(preemptor.PreemptionVictims) > 0 {
			return scheduler.ErrConflict
		}
		seen := make(map[string]struct{}, len(victimIDs))
		for _, victimID := range victimIDs {
			if _, duplicate := seen[victimID]; duplicate {
				return scheduler.ErrConflict
			}
			seen[victimID] = struct{}{}
			victim, err := jobFromBucket(tx, victimID)
			if err != nil {
				return err
			}
			if victim.NodeID != nodeID || victim.Priority >= preemptor.Priority ||
				(victim.Status != domain.JobStatusAssigned && victim.Status != domain.JobStatusRunning) ||
				victim.PreemptedByJobID != "" {
				return scheduler.ErrConflict
			}
			victim.Status, victim.Reason = domain.JobStatusCanceling, reason
			victim.CancelGracePeriod, victim.PreemptedByJobID = grace, preemptorID
			victim.PreemptionCount++
			victim.UpdatedAt = now.UTC()
			if err := putJob(tx, victim); err != nil {
				return err
			}
			if err := putAudit(tx, domain.AuditEvent{
				ID:        "preemption_" + preemptorID + "_" + victim.ID,
				ActorType: "scheduler", ActorID: "xirang-controller",
				Action: "job.preemption.requested", TargetKind: "job", TargetID: victim.ID,
				Outcome: "accepted", OccurredAt: now.UTC(),
				Details: map[string]string{
					"preemptorJobId":    preemptorID,
					"nodeId":            nodeID,
					"victimPriority":    fmt.Sprint(victim.Priority),
					"preemptorPriority": fmt.Sprint(preemptor.Priority),
				},
			}); err != nil {
				return err
			}
			requested = append(requested, victim)
		}
		preemptor.PreemptionNodeID = nodeID
		preemptor.PreemptionVictims = append([]string(nil), victimIDs...)
		preemptor.Reason = "waiting for lower-priority assignments to terminate on node " + nodeID
		preemptor.UpdatedAt = now.UTC()
		return putJob(tx, preemptor)
	})
	return requested, err
}

func (store *Store) RejectJobCancellation(ctx context.Context, assignmentID, reason string, now time.Time) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var updated domain.Job
	err := store.database.Update(func(tx *bbolt.Tx) error {
		jobID := tx.Bucket(jobAssignmentsBucket).Get([]byte(assignmentID))
		if jobID == nil {
			return scheduler.ErrNotFound
		}
		job, err := jobFromBucket(tx, string(jobID))
		if err != nil {
			return err
		}
		if job.Status != domain.JobStatusCanceling || job.AssignmentID != assignmentID {
			if job.Allocation == nil {
				return scheduler.ErrConflict
			}
		}
		if job.Allocation != nil {
			member, ok := job.Allocation.MemberByAssignment(assignmentID)
			if !ok || job.Status != domain.JobStatusCanceling {
				return scheduler.ErrConflict
			}
			job.Status = domain.JobStatusOrphaned
			job.Reason = fmt.Sprintf("gang cancellation rejected by node %s; reservations retained: %s", member.NodeID, reason)
			job.UpdatedAt, job.Allocation.UpdatedAt = now.UTC(), now.UTC()
			if err := putJob(tx, job); err != nil {
				return err
			}
			updated = job
			return nil
		}
		if job.PreemptedByJobID != "" {
			preemptorID := job.PreemptedByJobID
			job.Status = domain.JobStatusAssigned
			if job.StartedAt != nil {
				job.Status = domain.JobStatusRunning
			}
			job.Reason, job.CancelGracePeriod, job.PreemptedByJobID = reason, 0, ""
			job.UpdatedAt = now.UTC()
			if err := removePreemptionVictim(tx, preemptorID, job.ID, now, false); err != nil {
				return err
			}
			if err := putAudit(tx, domain.AuditEvent{
				ID:        "preemption_rejected_" + preemptorID + "_" + job.ID,
				ActorType: "agent", ActorID: job.NodeID, Action: "job.preemption.rejected",
				TargetKind: "job", TargetID: job.ID, Outcome: "rejected", OccurredAt: now.UTC(),
				Details: map[string]string{"preemptorJobId": preemptorID, "reason": reason},
			}); err != nil {
				return err
			}
			if err := putJob(tx, job); err != nil {
				return err
			}
			updated = job
			return nil
		}
		if err := releaseJobResources(tx, job, now); err != nil {
			return err
		}
		finished := now.UTC()
		job.Status, job.Reason, job.UpdatedAt, job.FinishedAt = domain.JobStatusFailed, reason, finished, &finished
		if err := putJob(tx, job); err != nil {
			return err
		}
		updated = job
		return nil
	})
	return updated, err
}

func (store *Store) finishUndeliveredAssignment(ctx context.Context, jobID, assignmentID, reason string, now time.Time, rejected bool) (domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, err
	}
	var updated domain.Job
	err := store.database.Update(func(tx *bbolt.Tx) error {
		job, err := jobFromBucket(tx, jobID)
		if err != nil {
			return err
		}
		if job.Allocation != nil {
			member, ok := job.Allocation.MemberByAssignment(assignmentID)
			if !rejected || !ok {
				return scheduler.ErrConflict
			}
			failed, changed, err := applyGangAssignmentReport(tx, job, domain.AssignmentReport{
				NodeID: member.NodeID, AssignmentID: assignmentID, Attempt: job.Attempt,
				State: domain.AssignmentStateFailed, Reason: reason, OccurredAt: now.UTC(), ReceivedAt: now.UTC(),
			})
			if err != nil {
				return err
			}
			if !changed {
				return scheduler.ErrConflict
			}
			updated = failed
			return nil
		}
		canceling := job.Status == domain.JobStatusCanceling
		if (job.Status != domain.JobStatusAssigned && !(rejected && canceling)) || job.AssignmentID != assignmentID {
			return scheduler.ErrConflict
		}
		if err := releaseJobResources(tx, job, now); err != nil {
			return err
		}
		if err := tx.Bucket(jobAssignmentsBucket).Delete([]byte(assignmentID)); err != nil {
			return err
		}
		job.AssignmentID, job.NodeID = "", ""
		job.Reason, job.UpdatedAt = reason, now.UTC()
		if rejected && canceling && job.PreemptedByJobID != "" {
			preemptorID := job.PreemptedByJobID
			job.Status, job.PreemptedByJobID, job.CancelGracePeriod = domain.JobStatusQueued, "", 0
			job.Reason = "preempted before execution; safely requeued"
			if err := removePreemptionVictim(tx, preemptorID, job.ID, now, true); err != nil {
				return err
			}
		} else if rejected && canceling {
			finished := now.UTC()
			job.Status, job.FinishedAt = domain.JobStatusCanceled, &finished
		} else if rejected {
			finished := now.UTC()
			job.Status, job.FinishedAt = domain.JobStatusFailed, &finished
		} else {
			job.Status = domain.JobStatusQueued
			if job.Attempt > 0 {
				job.Attempt--
			}
		}
		if err := putJob(tx, job); err != nil {
			return err
		}
		updated = job
		return nil
	})
	return updated, err
}

func (store *Store) ApplyAssignmentReport(ctx context.Context, report domain.AssignmentReport) (domain.Job, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, false, err
	}
	var updated domain.Job
	changed := false
	err := store.database.Update(func(tx *bbolt.Tx) error {
		jobID := tx.Bucket(jobAssignmentsBucket).Get([]byte(report.AssignmentID))
		if jobID == nil {
			return nil
		}
		job, err := jobFromBucket(tx, string(jobID))
		if err != nil {
			return err
		}
		if job.Allocation != nil {
			member, ok := job.Allocation.MemberByAssignment(report.AssignmentID)
			if !ok || member.NodeID != report.NodeID || job.Attempt != report.Attempt || job.Status.Terminal() {
				return nil
			}
		} else if job.AssignmentID != report.AssignmentID || job.NodeID != report.NodeID || job.Attempt != report.Attempt || job.Status.Terminal() {
			return nil
		}
		reportChanged, err := upsertAssignmentReport(tx, report)
		if err != nil {
			return err
		}
		if job.Allocation != nil {
			gangJob, gangChanged, gangErr := applyGangAssignmentReport(tx, job, report)
			updated, changed = gangJob, gangChanged
			return gangErr
		}
		if !reportChanged && job.Status != domain.JobStatusOrphaned {
			return nil
		}
		canceling := job.Status == domain.JobStatusCanceling
		job.UpdatedAt, job.ExitCode = report.ReceivedAt.UTC(), report.ExitCode
		if len(report.StdoutTail) > 0 || len(report.StderrTail) > 0 || report.LogsTruncated || report.State.Terminal() {
			logsObservedAt := report.OccurredAt.UTC()
			if job.LogsObservedAt == nil || logsObservedAt.After(*job.LogsObservedAt) {
				job.StdoutTail, job.StderrTail, job.LogsTruncated = report.StdoutTail, report.StderrTail, report.LogsTruncated
				job.LogsObservedAt = &logsObservedAt
			}
		}
		if report.Reason != "" || !canceling {
			job.Reason = report.Reason
		}
		if job.PreemptedByJobID != "" &&
			(report.State == domain.AssignmentStateCanceled || report.State == domain.AssignmentStateFailed) {
			preemptorID := job.PreemptedByJobID
			if err := releaseJobResources(tx, job, report.ReceivedAt); err != nil {
				return err
			}
			if err := tx.Bucket(jobAssignmentsBucket).Delete([]byte(job.AssignmentID)); err != nil {
				return err
			}
			job.Status, job.AssignmentID, job.NodeID = domain.JobStatusQueued, "", ""
			job.CancelGracePeriod, job.PreemptedByJobID = 0, ""
			job.FinishedAt = nil
			job.Reason = "preempted; safely requeued after Agent termination confirmation"
			if err := removePreemptionVictim(tx, preemptorID, job.ID, report.ReceivedAt, true); err != nil {
				return err
			}
			if err := putAudit(tx, domain.AuditEvent{
				ID:        "preemption_confirmed_" + preemptorID + "_" + job.ID,
				ActorType: "agent", ActorID: report.NodeID, Action: "job.preemption.confirmed",
				TargetKind: "job", TargetID: job.ID, Outcome: "succeeded", OccurredAt: report.ReceivedAt.UTC(),
				Details: map[string]string{"preemptorJobId": preemptorID, "assignmentId": report.AssignmentID},
			}); err != nil {
				return err
			}
			if err := putJob(tx, job); err != nil {
				return err
			}
			updated, changed = job, true
			return nil
		}
		switch report.State {
		case domain.AssignmentStateAccepted, domain.AssignmentStatePulling:
			if !canceling {
				job.Status = domain.JobStatusAssigned
			}
		case domain.AssignmentStateRunning:
			if !canceling {
				job.Status = domain.JobStatusRunning
			}
			if job.StartedAt == nil {
				started := report.OccurredAt.UTC()
				job.StartedAt = &started
			}
		case domain.AssignmentStateSuspended:
			if !canceling {
				job.Status = domain.JobStatusSuspended
			}
			if job.StartedAt == nil {
				started := report.OccurredAt.UTC()
				job.StartedAt = &started
			}
		case domain.AssignmentStateSucceeded:
			job.Status = domain.JobStatusSucceeded
		case domain.AssignmentStateCanceled:
			job.Status = domain.JobStatusCanceled
		case domain.AssignmentStateFailed:
			if !canceling && int64(job.Attempt) < int64(job.MaxAttempts)+int64(job.PreemptionCount) {
				if err := releaseJobResources(tx, job, report.ReceivedAt); err != nil {
					return err
				}
				job.Status, job.AssignmentID, job.NodeID = domain.JobStatusQueued, "", ""
				if err := putJob(tx, job); err != nil {
					return err
				}
				updated, changed = job, true
				return nil
			}
			job.Status = domain.JobStatusFailed
		}
		if job.PreemptedByJobID != "" && job.Status.Terminal() {
			preemptorID := job.PreemptedByJobID
			job.PreemptedByJobID, job.CancelGracePeriod = "", 0
			if err := removePreemptionVictim(tx, preemptorID, job.ID, report.ReceivedAt, true); err != nil {
				return err
			}
		}
		if job.Status.Terminal() {
			if err := releaseJobResources(tx, job, report.ReceivedAt); err != nil {
				return err
			}
			finished := report.OccurredAt.UTC()
			job.FinishedAt = &finished
		}
		if err := putJob(tx, job); err != nil {
			return err
		}
		updated, changed = job, true
		return nil
	})
	return updated, changed, err
}

func applyGangAssignmentReport(tx *bbolt.Tx, job domain.Job, report domain.AssignmentReport) (domain.Job, bool, error) {
	member, ok := job.Allocation.MemberByAssignment(report.AssignmentID)
	if !ok || member.NodeID != report.NodeID || job.Attempt != report.Attempt || job.Status.Terminal() {
		return domain.Job{}, false, nil
	}
	if member.ObservedAt != nil {
		currentProgress, incomingProgress := domain.AssignmentProgress(member.State), domain.AssignmentProgress(report.State)
		if incomingProgress < currentProgress || (member.State.Terminal() && report.State != member.State) ||
			(incomingProgress == currentProgress && !report.OccurredAt.After(*member.ObservedAt)) {
			return domain.Job{}, false, nil
		}
	}
	observedAt := report.OccurredAt.UTC()
	member.State, member.ExitCode, member.Reason, member.ObservedAt = report.State, report.ExitCode, report.Reason, &observedAt
	if len(report.StdoutTail) > 0 || len(report.StderrTail) > 0 || report.LogsTruncated || report.State.Terminal() {
		member.StdoutTail, member.StderrTail, member.LogsTruncated = report.StdoutTail, report.StderrTail, report.LogsTruncated
		job.StdoutTail, job.StderrTail, job.LogsTruncated = report.StdoutTail, report.StderrTail, report.LogsTruncated
		job.LogsObservedAt = &observedAt
	}
	job.UpdatedAt, job.Allocation.UpdatedAt = report.ReceivedAt.UTC(), report.ReceivedAt.UTC()
	if report.State == domain.AssignmentStateFailed && job.Allocation.State != domain.AllocationStateCanceling {
		job.Allocation.State = domain.AllocationStateCanceling
		job.Status = domain.JobStatusCanceling
		job.ExitCode = report.ExitCode
		job.Reason = fmt.Sprintf("gang member %s failed: %s", member.NodeID, report.Reason)
	}
	if report.State == domain.AssignmentStateReady && allocationMembersAll(job.Allocation, func(state domain.AssignmentState) bool {
		return state == domain.AssignmentStateReady
	}) {
		job.Allocation.State = domain.AllocationStateReady
		job.Reason = "all gang members prepared; releasing start barrier"
	}
	if report.State == domain.AssignmentStateRunning {
		job.Allocation.State = domain.AllocationStateRunning
		if job.Status != domain.JobStatusCanceling {
			job.Status, job.Reason = domain.JobStatusRunning, ""
		}
		if job.StartedAt == nil {
			started := observedAt
			job.StartedAt = &started
		}
	}
	if !allocationMembersAll(job.Allocation, func(state domain.AssignmentState) bool { return state.Terminal() }) {
		if err := putJob(tx, job); err != nil {
			return domain.Job{}, false, err
		}
		return job, true, nil
	}
	failed := false
	allSucceeded := true
	for _, candidate := range job.Allocation.Members {
		failed = failed || candidate.State == domain.AssignmentStateFailed
		allSucceeded = allSucceeded && candidate.State == domain.AssignmentStateSucceeded
	}
	if err := releaseAllocationResources(tx, job, report.ReceivedAt); err != nil {
		return domain.Job{}, false, err
	}
	for _, candidate := range job.Allocation.Members {
		if err := tx.Bucket(jobAssignmentsBucket).Delete([]byte(candidate.AssignmentID)); err != nil {
			return domain.Job{}, false, err
		}
	}
	finished := observedAt
	job.FinishedAt = &finished
	switch {
	case allSucceeded:
		job.Status, job.Allocation.State, job.Reason = domain.JobStatusSucceeded, domain.AllocationStateSucceeded, ""
	case failed && int64(job.Attempt) < int64(job.MaxAttempts)+int64(job.PreemptionCount):
		job.Status, job.AssignmentID, job.NodeID, job.Allocation = domain.JobStatusQueued, "", "", nil
		job.FinishedAt = nil
		job.Reason = "gang allocation failed; safely requeued after all members terminated"
	case failed:
		job.Status, job.Allocation.State = domain.JobStatusFailed, domain.AllocationStateFailed
		if job.Reason == "" {
			job.Reason = "one or more gang members failed"
		}
	default:
		job.Status, job.Allocation.State = domain.JobStatusCanceled, domain.AllocationStateCanceled
		if job.Reason == "" {
			job.Reason = "gang allocation canceled"
		}
	}
	if err := putJob(tx, job); err != nil {
		return domain.Job{}, false, err
	}
	return job, true, nil
}

func allocationMembersAll(allocation *domain.Allocation, predicate func(domain.AssignmentState) bool) bool {
	if allocation == nil || len(allocation.Members) == 0 {
		return false
	}
	for _, member := range allocation.Members {
		if !predicate(member.State) {
			return false
		}
	}
	return true
}

func (store *Store) ApplyAssignmentLog(ctx context.Context, report domain.AssignmentLogSnapshot) (domain.Job, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Job{}, false, err
	}
	var updated domain.Job
	changed := false
	err := store.database.Update(func(tx *bbolt.Tx) error {
		jobID := tx.Bucket(jobAssignmentsBucket).Get([]byte(report.AssignmentID))
		if jobID == nil {
			return nil
		}
		job, err := jobFromBucket(tx, string(jobID))
		if err != nil {
			return err
		}
		if job.Allocation != nil {
			member, ok := job.Allocation.MemberByAssignment(report.AssignmentID)
			if !ok || member.NodeID != report.NodeID || job.Attempt != report.Attempt ||
				(member.LogsObservedAt != nil && !report.ObservedAt.After(*member.LogsObservedAt)) {
				return nil
			}
			observedAt := report.ObservedAt.UTC()
			member.StdoutTail, member.StderrTail, member.LogsTruncated = report.StdoutTail, report.StderrTail, report.LogsTruncated
			member.LogsObservedAt = &observedAt
			job.StdoutTail, job.StderrTail, job.LogsTruncated = report.StdoutTail, report.StderrTail, report.LogsTruncated
			job.LogsObservedAt, job.UpdatedAt, job.Allocation.UpdatedAt = &observedAt, report.ReceivedAt.UTC(), report.ReceivedAt.UTC()
			if err := putJob(tx, job); err != nil {
				return err
			}
			updated, changed = job, true
			return nil
		}
		if job.AssignmentID != report.AssignmentID || job.NodeID != report.NodeID || job.Attempt != report.Attempt ||
			(job.LogsObservedAt != nil && !report.ObservedAt.After(*job.LogsObservedAt)) {
			return nil
		}
		observedAt := report.ObservedAt.UTC()
		job.StdoutTail, job.StderrTail, job.LogsTruncated = report.StdoutTail, report.StderrTail, report.LogsTruncated
		job.LogsObservedAt, job.UpdatedAt = &observedAt, report.ReceivedAt.UTC()
		if err := putJob(tx, job); err != nil {
			return err
		}
		updated, changed = job, true
		return nil
	})
	return updated, changed, err
}

func (store *Store) ReconcileOrphanedJobs(ctx context.Context, now time.Time) ([]domain.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	changed := make([]domain.Job, 0)
	err := store.database.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(jobsBucket).ForEach(func(_, encoded []byte) error {
			var job domain.Job
			if err := json.Unmarshal(encoded, &job); err != nil {
				return err
			}
			if job.Status != domain.JobStatusAssigned && job.Status != domain.JobStatusRunning && job.Status != domain.JobStatusSuspended {
				return nil
			}
			if job.Allocation != nil {
				for _, member := range job.Allocation.Members {
					node, err := nodeFromBucket(tx, member.NodeID)
					if err != nil {
						return err
					}
					if node.Status == domain.NodeStatusUnreachable || node.Status == domain.NodeStatusOffline {
						job.Status, job.Reason, job.UpdatedAt = domain.JobStatusOrphaned, "gang member unreachable; all reservations retained until Agent reconciliation", now.UTC()
						if err := putJob(tx, job); err != nil {
							return err
						}
						changed = append(changed, job)
						break
					}
				}
				return nil
			}
			node, err := nodeFromBucket(tx, job.NodeID)
			if err != nil {
				return err
			}
			if node.Status != domain.NodeStatusUnreachable && node.Status != domain.NodeStatusOffline {
				return nil
			}
			job.Status, job.Reason, job.UpdatedAt = domain.JobStatusOrphaned, "node unreachable; reservation retained until Agent reconciliation", now.UTC()
			if err := putJob(tx, job); err != nil {
				return err
			}
			changed = append(changed, job)
			return nil
		})
	})
	return changed, err
}

func upsertAssignmentReport(tx *bbolt.Tx, report domain.AssignmentReport) (bool, error) {
	key := []byte(fmt.Sprintf("%s\x00%s\x00%010d", report.NodeID, report.AssignmentID, report.Attempt))
	bucket := tx.Bucket(assignmentsBucket)
	if encoded := bucket.Get(key); encoded != nil {
		var current domain.AssignmentReport
		if err := json.Unmarshal(encoded, &current); err != nil {
			return false, err
		}
		currentProgress := domain.AssignmentProgress(current.State)
		incomingProgress := domain.AssignmentProgress(report.State)
		if incomingProgress < currentProgress || (current.State.Terminal() && report.State != current.State) ||
			(incomingProgress == currentProgress && !report.OccurredAt.After(current.OccurredAt)) {
			return false, nil
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return false, err
	}
	return true, bucket.Put(key, encoded)
}

func jobFromBucket(tx *bbolt.Tx, id string) (domain.Job, error) {
	encoded := tx.Bucket(jobsBucket).Get([]byte(id))
	if encoded == nil {
		return domain.Job{}, scheduler.ErrNotFound
	}
	var job domain.Job
	if err := json.Unmarshal(encoded, &job); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

func nodeFromBucket(tx *bbolt.Tx, id string) (domain.Node, error) {
	encoded := tx.Bucket(nodesBucket).Get([]byte(id))
	if encoded == nil {
		return domain.Node{}, scheduler.ErrNotFound
	}
	var record nodeRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return domain.Node{}, err
	}
	return record.Node, nil
}

func putJob(tx *bbolt.Tx, job domain.Job) error {
	encoded, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return tx.Bucket(jobsBucket).Put([]byte(job.ID), encoded)
}

func putNode(tx *bbolt.Tx, node domain.Node) error {
	encoded, err := json.Marshal(nodeRecord{Node: node})
	if err != nil {
		return err
	}
	return tx.Bucket(nodesBucket).Put([]byte(node.ID), encoded)
}

func releaseJobResources(tx *bbolt.Tx, job domain.Job, now time.Time) error {
	if job.Allocation != nil {
		return releaseAllocationResources(tx, job, now)
	}
	node, err := nodeFromBucket(tx, job.NodeID)
	if err != nil {
		return err
	}
	remaining, err := node.Allocated.Subtract(job.Task.Resources)
	if err != nil {
		return err
	}
	node.Allocated, node.UpdatedAt = remaining, now.UTC()
	node.Revision++
	return putNode(tx, node)
}

func releaseAllocationResources(tx *bbolt.Tx, job domain.Job, now time.Time) error {
	if job.Allocation == nil {
		return scheduler.ErrConflict
	}
	seen := make(map[string]bool, len(job.Allocation.Members))
	for _, member := range job.Allocation.Members {
		if seen[member.NodeID] {
			return scheduler.ErrConflict
		}
		seen[member.NodeID] = true
		node, err := nodeFromBucket(tx, member.NodeID)
		if err != nil {
			return err
		}
		remaining, err := node.Allocated.Subtract(job.Task.Resources)
		if err != nil {
			return err
		}
		node.Allocated, node.UpdatedAt = remaining, now.UTC()
		node.Revision++
		if err := putNode(tx, node); err != nil {
			return err
		}
	}
	return nil
}

func removePreemptionVictim(tx *bbolt.Tx, preemptorID, victimID string, now time.Time, targetReady bool) error {
	preemptor, err := jobFromBucket(tx, preemptorID)
	if err != nil {
		if errors.Is(err, scheduler.ErrNotFound) {
			return nil
		}
		return err
	}
	remaining := make([]string, 0, len(preemptor.PreemptionVictims))
	for _, id := range preemptor.PreemptionVictims {
		if id != victimID {
			remaining = append(remaining, id)
		}
	}
	preemptor.PreemptionVictims = remaining
	if len(remaining) == 0 {
		if !targetReady || preemptor.Status != domain.JobStatusQueued {
			preemptor.PreemptionNodeID = ""
		}
		if preemptor.Status == domain.JobStatusQueued {
			preemptor.Reason = ""
		}
	}
	preemptor.UpdatedAt = now.UTC()
	return putJob(tx, preemptor)
}

func putAudit(tx *bbolt.Tx, event domain.AuditEvent) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	key := event.OccurredAt.UTC().Format("20060102T150405.000000000Z") + "_" + event.ID
	return tx.Bucket(auditBucket).Put([]byte(key), encoded)
}
