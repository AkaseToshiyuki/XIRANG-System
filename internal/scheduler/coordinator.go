// Package scheduler owns durable jobs, priority ordering, resource reservations,
// and conversion from a scheduled Job to the existing Agent assignment protocol.
package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/runtimecompat"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrNotFound      = errors.New("job not found")
	ErrConflict      = errors.New("job state conflict")
	ErrInvalidJob    = errors.New("invalid job")
	ErrQuotaExceeded = errors.New("job quota exceeded")
	ErrNoCapacity    = errors.New("no schedulable node has sufficient resources")
	ErrUnsatisfiable = errors.New("job cannot be satisfied by the enrolled cluster")
)

type UnsatisfiableError struct {
	Reason domain.JobBlockReason
}

func (err *UnsatisfiableError) Error() string {
	return ErrUnsatisfiable.Error() + ": " + err.Reason.Message
}

func (err *UnsatisfiableError) Unwrap() error { return ErrUnsatisfiable }

type Repository interface {
	CreateJob(context.Context, domain.Job) error
	GetJob(context.Context, string) (domain.Job, error)
	ListJobs(context.Context, int) ([]domain.Job, error)
	ListAllJobs(context.Context) ([]domain.Job, error)
	ListActiveJobs(context.Context) ([]domain.Job, error)
	ListJobsFinishedSince(context.Context, time.Time) ([]domain.Job, error)
	ListJobsForOwner(context.Context, string, int) ([]domain.Job, error)
	ListActiveJobsForOwner(context.Context, string) ([]domain.Job, error)
	AssignJob(context.Context, string, string, string, time.Time) (domain.Job, error)
	RollbackAssignment(context.Context, string, string, string, time.Time) (domain.Job, error)
	RejectAssignment(context.Context, string, string, string, time.Time) (domain.Job, error)
	RequestJobCancellation(context.Context, string, string, time.Duration, time.Time) (domain.Job, bool, error)
	RequestJobPreemption(context.Context, string, string, []string, string, time.Duration, time.Time) ([]domain.Job, error)
	RejectJobCancellation(context.Context, string, string, time.Time) (domain.Job, error)
	ApplyAssignmentReport(context.Context, domain.AssignmentReport) (domain.Job, bool, error)
	ApplyAssignmentLog(context.Context, domain.AssignmentLogSnapshot) (domain.Job, bool, error)
	ApplyAssignmentUsage(context.Context, domain.AssignmentUsageSnapshot) (domain.Job, bool, error)
	FailQueuedJob(context.Context, string, string, time.Time) (domain.Job, error)
	ReconcileOrphanedJobs(context.Context, time.Time) ([]domain.Job, error)
	PutQueuePolicy(context.Context, domain.QueuePolicy) error
	ListQueuePolicies(context.Context) ([]domain.QueuePolicy, error)
}

type storageQuotaRepository interface {
	UpdateGroupJobStorageQuota(context.Context, string, int64, time.Time) ([]domain.Job, error)
}

type userStorageQuotaRepository interface {
	UpdateUserJobStorageQuota(context.Context, string, int64, time.Time) ([]domain.Job, error)
}

type groupDisableRepository interface {
	SetGroupJobsSuspended(context.Context, string, bool, string, time.Time) ([]domain.Job, error)
}

type gangRepository interface {
	AssignJobAllocation(context.Context, string, domain.Allocation, time.Time) (domain.Job, error)
	RollbackJobAllocation(context.Context, string, string, string, time.Time) (domain.Job, error)
}

type queuedBlockRepository interface {
	UpdateQueuedJobBlock(context.Context, string, *domain.JobBlockReason, time.Time) (domain.Job, error)
}

type NodeSource interface {
	ListNodes(context.Context) ([]domain.Node, error)
}

// BatchWeightModel allows deployments to replace XIRANG's enrollment-derived
// CPU/NVIDIA proxy when decomposing independent manifest items. A model must
// return a finite positive relative weight; invalid results fall back to the
// built-in conservative model.
type BatchWeightModel interface {
	Weight(domain.Node, domain.TaskSpec) float64
}

type BatchWeightFunc func(domain.Node, domain.TaskSpec) float64

func (function BatchWeightFunc) Weight(node domain.Node, task domain.TaskSpec) float64 {
	return function(node, task)
}

type TopologySource interface {
	LatestSnapshot(context.Context) (domain.TopologySnapshot, error)
}

type CommandQueue interface {
	Enqueue(string, *xirangv1.ControlMessage) error
}

type batchCommandQueue interface {
	EnqueueBatch(map[string][]*xirangv1.ControlMessage) error
}

type AlertSink interface {
	ReportCondition(context.Context, string, string, string, domain.AlertSeverity, string) (domain.Alert, error)
	ResolveCondition(context.Context, string, string) (domain.Alert, error)
}

type SubmitRequest struct {
	OwnerID        string
	OwnerName      string
	GroupID        string
	PolicyGroupIDs []string
	ProjectID      string
	Queue          string
	Priority       int32
	Task           domain.TaskSpec
	MaxAttempts    int32
	// StorageQuotaBytes is administrator policy captured from the owner's group.
	// Zero means unlimited; it is never populated from a job script.
	StorageQuotaBytes           int64
	ArrayID                     string
	ArrayIndex                  int32
	ArraySize                   int32
	Dependencies                []domain.JobDependency
	AllowCrossOwnerDependencies bool
}

type CancelRequest struct {
	Reason      string
	GracePeriod time.Duration
}

type Coordinator struct {
	repository      Repository
	nodes           NodeSource
	commands        CommandQueue
	now             func() time.Time
	random          func([]byte) (int, error)
	wake            chan struct{}
	topology        TopologySource
	batchWeights    BatchWeightModel
	storageMu       sync.Mutex
	storagePolicies map[string]uint64
	alerts          AlertSink
}

func (coordinator *Coordinator) SetAlertSink(sink AlertSink) {
	coordinator.alerts = sink
}

func (coordinator *Coordinator) SetTopologySource(source TopologySource) {
	coordinator.topology = source
}

// SetBatchWeightModel installs a workload-specific apportionment model. It is
// intended to be configured before the coordinator starts accepting work.
func (coordinator *Coordinator) SetBatchWeightModel(model BatchWeightModel) {
	coordinator.batchWeights = model
}

func New(repository Repository, nodes NodeSource, commands CommandQueue) *Coordinator {
	return &Coordinator{
		repository: repository, nodes: nodes, commands: commands,
		now: time.Now, random: rand.Read, wake: make(chan struct{}, 1), storagePolicies: make(map[string]uint64),
	}
}

func (coordinator *Coordinator) Submit(ctx context.Context, request SubmitRequest) (domain.Job, error) {
	if err := validateSubmission(request); err != nil {
		return domain.Job{}, err
	}
	if coordinator.nodes != nil {
		nodes, err := coordinator.nodes.ListNodes(ctx)
		if err != nil {
			return domain.Job{}, err
		}
		if reason := permanentlyUnsatisfiableReason(request.Task, nodes); reason != nil {
			return domain.Job{}, &UnsatisfiableError{Reason: *reason}
		}
	}
	identifier, err := randomID(coordinator.random, "job_")
	if err != nil {
		return domain.Job{}, err
	}
	now := coordinator.now().UTC()
	queue := strings.TrimSpace(request.Queue)
	if queue == "" {
		queue = "default"
	}
	maxAttempts := request.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 1
	}
	task := cloneTask(request.Task)
	if request.ArraySize > 0 {
		if task.Environment == nil {
			task.Environment = map[string]string{}
		}
		task.Environment["XIRANG_ARRAY_ID"] = request.ArrayID
		task.Environment["XIRANG_ARRAY_INDEX"] = strconv.FormatInt(int64(request.ArrayIndex), 10)
		task.Environment["XIRANG_ARRAY_SIZE"] = strconv.FormatInt(int64(request.ArraySize), 10)
	}
	for _, dependency := range request.Dependencies {
		prerequisite, dependencyErr := coordinator.repository.GetJob(ctx, dependency.JobID)
		if dependencyErr != nil {
			return domain.Job{}, fmt.Errorf("%w: dependency %s does not exist", ErrInvalidJob, dependency.JobID)
		}
		if prerequisite.OwnerID != strings.TrimSpace(request.OwnerID) && !request.AllowCrossOwnerDependencies {
			return domain.Job{}, fmt.Errorf("%w: dependency is not owned by the submitting user", ErrInvalidJob)
		}
	}
	job := domain.Job{
		ID: identifier, OwnerID: strings.TrimSpace(request.OwnerID), OwnerName: strings.TrimSpace(request.OwnerName),
		GroupID: strings.TrimSpace(request.GroupID), PolicyGroupIDs: append([]string(nil), request.PolicyGroupIDs...),
		ProjectID: strings.TrimSpace(request.ProjectID), Queue: queue,
		Priority: request.Priority, Status: domain.JobStatusQueued, Task: task,
		MaxAttempts: maxAttempts, StorageQuotaBytes: request.StorageQuotaBytes, CreatedAt: now, UpdatedAt: now,
		ArrayID: request.ArrayID, ArrayIndex: request.ArrayIndex, ArraySize: request.ArraySize,
		Dependencies: append([]domain.JobDependency(nil), request.Dependencies...),
	}
	if len(job.Dependencies) > 0 {
		job.Reason = "waiting for declared job dependencies"
		job.BlockReason = &domain.JobBlockReason{
			Code: "dependency_wait", Category: "dependency", Message: job.Reason,
			Details: map[string]string{"dependencyCount": strconv.Itoa(len(job.Dependencies))}, ObservedAt: now,
		}
	}
	if err := coordinator.repository.CreateJob(ctx, job); err != nil {
		return domain.Job{}, err
	}
	coordinator.notify()
	return job, nil
}

// PlanBatch returns one pinned node ID per child job. It chooses at most
// maxNodes compatible nodes, then applies largest-remainder apportionment to
// their measured compute weights. Queueing and retries remain independent for
// every child, which keeps a large parameter sweep observable and cancellable.
func (coordinator *Coordinator) PlanBatch(ctx context.Context, task domain.TaskSpec, itemCount, maxNodes int) ([]string, error) {
	if itemCount < 1 || itemCount > 1000 || maxNodes < 1 || maxNodes > 128 || coordinator.nodes == nil {
		return nil, ErrInvalidJob
	}
	nodes, err := coordinator.nodes.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	job := domain.Job{Task: task}
	candidates := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if nodeMatchesJob(node, job) && node.Allocatable.Fits(task.Resources) {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 {
		return nil, &UnsatisfiableError{Reason: *blockReason("batch_nodes_unavailable", "resource", "no enrolled compute node can run this batch application", nil)}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ComputePriority != candidates[j].ComputePriority {
			return candidates[i].ComputePriority > candidates[j].ComputePriority
		}
		left, right := coordinator.batchWeight(candidates[i], task), coordinator.batchWeight(candidates[j], task)
		if left != right {
			return left > right
		}
		return candidates[i].ID < candidates[j].ID
	})
	count := min(maxNodes, len(candidates), itemCount)
	candidates = candidates[:count]
	allocation := make([]int, count)
	remaining := itemCount
	if itemCount >= count {
		for index := range allocation {
			allocation[index] = 1
		}
		remaining -= count
	}
	totalWeight := 0.0
	for _, node := range candidates {
		totalWeight += coordinator.batchWeight(node, task)
	}
	type remainder struct {
		index int
		value float64
	}
	remainders := make([]remainder, 0, count)
	assigned := 0
	for index, node := range candidates {
		exact := float64(remaining) * coordinator.batchWeight(node, task) / totalWeight
		whole := int(math.Floor(exact))
		allocation[index] += whole
		assigned += whole
		remainders = append(remainders, remainder{index: index, value: exact - float64(whole)})
	}
	sort.SliceStable(remainders, func(i, j int) bool {
		if remainders[i].value != remainders[j].value {
			return remainders[i].value > remainders[j].value
		}
		return candidates[remainders[i].index].ID < candidates[remainders[j].index].ID
	})
	for index := 0; index < remaining-assigned; index++ {
		allocation[remainders[index%len(remainders)].index]++
	}
	result := make([]string, 0, itemCount)
	for index, quantity := range allocation {
		for item := 0; item < quantity; item++ {
			result = append(result, candidates[index].ID)
		}
	}
	return result, nil
}

func (coordinator *Coordinator) batchWeight(node domain.Node, task domain.TaskSpec) float64 {
	if coordinator.batchWeights != nil {
		weight := coordinator.batchWeights.Weight(node, task)
		if weight > 0 && !math.IsNaN(weight) && !math.IsInf(weight, 0) {
			return weight
		}
	}
	return taskComputeWeight(node, task)
}

// taskComputeWeight keeps CPU-only work from being biased by an NVIDIA score
// that happens to be present in the node's aggregate inventory weight. The
// current Agent reports ComputeWeight as CPU benchmark + NVIDIA SM score, so
// GPU work may use the aggregate value while every other workload is weighted
// by CPU capacity alone.
func taskComputeWeight(node domain.Node, task domain.TaskSpec) float64 {
	if task.Resources.Devices["nvidia.com/gpu"] > 0 && node.ComputeWeight > 0 {
		return node.ComputeWeight
	}
	if node.CPUBenchmarkScore > 0 {
		return node.CPUBenchmarkScore
	}
	if node.Allocatable.MilliCPU > 0 {
		return float64(node.Allocatable.MilliCPU) / 1000
	}
	return 1
}

func permanentlyUnsatisfiableReason(task domain.TaskSpec, nodes []domain.Node) *domain.JobBlockReason {
	enrolled := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if !node.HasRole(domain.NodeRoleCompute) || node.Status == domain.NodeStatusDiscovered ||
			node.Status == domain.NodeStatusPendingApproval || node.Status == domain.NodeStatusRejected {
			continue
		}
		enrolled = append(enrolled, node)
	}
	// An empty cluster is allowed to accept work before its first compute node
	// enrolls. Once inventory exists, impossible per-node requests fail fast.
	if len(enrolled) == 0 {
		return nil
	}
	requiredArchitecture := task.Placement.RequiredArchitecture
	if requiredArchitecture == "" {
		requiredArchitecture = task.Artifact.Architecture
	}
	compatible := make([]domain.Node, 0, len(enrolled))
	for _, node := range enrolled {
		artifact, available := artifactForNode(task, node.ID)
		if !available || (task.Placement.RequiredNodeID != "" && task.Placement.RequiredNodeID != node.ID) {
			continue
		}
		nodeArchitecture := requiredArchitecture
		if nodeArchitecture == "" {
			nodeArchitecture = artifact.Architecture
		}
		if nodeArchitecture == "" || node.Architecture == nodeArchitecture {
			compatible = append(compatible, node)
		}
	}
	details := resourceDetails(task.Resources, map[string]string{
		"enrolledComputeNodes": strconv.Itoa(len(enrolled)),
		"requiredArchitecture": requiredArchitecture,
		"nodesPerAllocation":   strconv.FormatInt(int64(task.Parallel.Effective().Nodes), 10),
	})
	if len(compatible) == 0 {
		details["compatibleNodes"] = "0"
		return blockReason("architecture_capacity_unavailable", "resource", "no enrolled compute node matches the requested architecture", details)
	}
	var largestCPU, largestMemory int64
	capableNodes := 0
	for _, node := range compatible {
		largestCPU = max(largestCPU, node.Allocatable.MilliCPU)
		largestMemory = max(largestMemory, node.Allocatable.MemoryBytes)
		if node.Allocatable.Fits(task.Resources) {
			capableNodes++
		}
	}
	requiredNodes := int(task.Parallel.Effective().Nodes)
	if capableNodes >= requiredNodes {
		return nil
	}
	details["compatibleNodes"] = strconv.Itoa(len(compatible))
	details["capableNodes"] = strconv.Itoa(capableNodes)
	details["largestNodeMilliCPU"] = strconv.FormatInt(largestCPU, 10)
	details["largestNodeMemoryBytes"] = strconv.FormatInt(largestMemory, 10)
	if capableNodes > 0 {
		return blockReason("allocation_node_count_impossible", "resource", "too few enrolled compute nodes can satisfy the requested per-node resources", details)
	}
	return blockReason("per_node_capacity_impossible", "resource", "requested per-node resources exceed every enrolled compute node's total capacity", details)
}

func (coordinator *Coordinator) Get(ctx context.Context, id string) (domain.Job, error) {
	if !validText(id, 256, false) {
		return domain.Job{}, ErrInvalidJob
	}
	return coordinator.repository.GetJob(ctx, id)
}

func (coordinator *Coordinator) List(ctx context.Context, limit int) ([]domain.Job, error) {
	if limit < 1 || limit > 10_000 {
		return nil, ErrInvalidJob
	}
	return coordinator.repository.ListJobs(ctx, limit)
}

// ListForOwner returns a bounded page of one owner's jobs. Callers that must
// not expose other owners' work use this instead of filtering the result of
// List, which would silently shrink the page to nothing on a busy cluster.
func (coordinator *Coordinator) ListForOwner(ctx context.Context, ownerID string, limit int) ([]domain.Job, error) {
	if limit < 1 || limit > 10_000 {
		return nil, ErrInvalidJob
	}
	return coordinator.repository.ListJobsForOwner(ctx, ownerID, limit)
}

// ListActiveForOwner returns every non-terminal job an owner currently holds,
// which is what quota accounting has to see in full to stay correct.
func (coordinator *Coordinator) ListActiveForOwner(ctx context.Context, ownerID string) ([]domain.Job, error) {
	return coordinator.repository.ListActiveJobsForOwner(ctx, ownerID)
}

// ListActive returns every non-terminal job in the cluster.
func (coordinator *Coordinator) ListActive(ctx context.Context) ([]domain.Job, error) {
	return coordinator.repository.ListActiveJobs(ctx)
}

func (coordinator *Coordinator) PutQueuePolicy(ctx context.Context, policy domain.QueuePolicy) (domain.QueuePolicy, error) {
	policy.Name = strings.TrimSpace(policy.Name)
	if !validText(policy.Name, 128, false) || policy.Weight < 0 || policy.Weight > 1000 || policy.MaxConcurrent < 0 ||
		policy.MaxConcurrent > 1_000_000 || policy.MaxConcurrentPerProject < 0 || policy.MaxConcurrentPerProject > 1_000_000 ||
		policy.ResourceQuota.Validate() != nil || policy.ProjectResourceQuota.Validate() != nil ||
		policy.PreemptionGraceSeconds < 0 || policy.PreemptionGraceSeconds > 300 ||
		policy.MaxPreemptionsPerJob < 0 || policy.MaxPreemptionsPerJob > 100 ||
		policy.MaxRecommendedScaleOut < 0 || policy.MaxRecommendedScaleOut > 10_000 ||
		policy.ElasticityBudget.Validate() != nil ||
		policy.FairShareHalfLifeHours < 0 || policy.FairShareHalfLifeHours > 24*365 ||
		policy.HistoricalUsageWeight < 0 || policy.HistoricalUsageWeight > 1000 ||
		len(policy.ReservationNodeIDs) > 1024 || len(policy.ReservationGroupIDs) > 1024 ||
		(policy.QoS != "" && policy.QoS != "normal" && policy.QoS != "high" && policy.QoS != "urgent") {
		return domain.QueuePolicy{}, ErrInvalidJob
	}
	if (policy.ReservationStart == nil) != (policy.ReservationEnd == nil) ||
		(policy.ReservationStart != nil && !policy.ReservationEnd.After(*policy.ReservationStart)) {
		return domain.QueuePolicy{}, ErrInvalidJob
	}
	if policy.Weight == 0 {
		policy.Weight = 1
	}
	policy.UpdatedAt = coordinator.now().UTC()
	if err := coordinator.repository.PutQueuePolicy(ctx, policy); err != nil {
		return domain.QueuePolicy{}, err
	}
	coordinator.notify()
	return policy, nil
}

func (coordinator *Coordinator) ListQueuePolicies(ctx context.Context) ([]domain.QueuePolicy, error) {
	return coordinator.repository.ListQueuePolicies(ctx)
}

func (coordinator *Coordinator) Cancel(ctx context.Context, id string, request CancelRequest) (domain.Job, error) {
	if !validText(id, 256, false) || !validText(request.Reason, 4096, true) || request.GracePeriod < 0 || request.GracePeriod > 5*time.Minute {
		return domain.Job{}, ErrInvalidJob
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = "canceled by administrator"
	}
	job, dispatch, err := coordinator.repository.RequestJobCancellation(ctx, id, boundedReason(reason), request.GracePeriod, coordinator.now().UTC())
	if err != nil || !dispatch {
		return job, err
	}
	if err := coordinator.enqueueJobCancellations(job); err != nil {
		return job, err
	}
	return job, nil
}

// Run schedules on wakeups and periodically reconciles work submitted by another
// controller process before the single-controller file lock was acquired.
func (coordinator *Coordinator) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := coordinator.Schedule(ctx); err != nil && !errors.Is(err, context.Canceled) && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-coordinator.wake:
		case <-ticker.C:
		}
	}
}

func (coordinator *Coordinator) notify() {
	select {
	case coordinator.wake <- struct{}{}:
	default:
	}
}

func (coordinator *Coordinator) Schedule(ctx context.Context) error {
	if coordinator.repository == nil || coordinator.nodes == nil || coordinator.commands == nil {
		return errors.New("scheduler dependencies are unavailable")
	}
	if _, err := coordinator.repository.ReconcileOrphanedJobs(ctx, coordinator.now().UTC()); err != nil {
		return err
	}
	policies, err := coordinator.repository.ListQueuePolicies(ctx)
	if err != nil {
		return err
	}
	policyByQueue := make(map[string]domain.QueuePolicy, len(policies))
	for _, policy := range policies {
		policyByQueue[policy.Name] = policy
	}
	// Scheduling needs the live working set plus the finished jobs that still
	// carry weight, not the entire history. Fair-share debt decays by half over
	// each configured half-life, so beyond fairShareHorizon a completed job
	// contributes less than a thousandth of its original weight and reading it
	// only costs time that grows with the age of the cluster.
	jobs, err := coordinator.repository.ListActiveJobs(ctx)
	if err != nil {
		return err
	}
	recent, err := coordinator.repository.ListJobsFinishedSince(ctx, coordinator.now().UTC().Add(-fairShareHorizon(policies)))
	if err != nil {
		return err
	}
	jobs = append(jobs, recent...)
	for _, job := range jobs {
		if job.Status == domain.JobStatusCanceling && job.AssignmentID != "" && job.NodeID != "" {
			if err := coordinator.enqueueJobCancellations(job); err != nil {
				return err
			}
			continue
		}
	}
	stats := buildSchedulingStats(jobs, policyByQueue, coordinator.now().UTC())
	pending := make([]domain.Job, 0)
	jobsByID := make(map[string]domain.Job, len(jobs))
	for _, job := range jobs {
		jobsByID[job.ID] = job
	}
	// dependencyState fails a job outright when a prerequisite is missing from
	// this map, so a prerequisite that finished before the window above has to
	// be fetched. Dependencies are declared by identifier, which makes this a
	// bounded set of point reads rather than another scan.
	if err := coordinator.resolveDeclaredDependencies(ctx, jobs, jobsByID); err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Status == domain.JobStatusQueued {
			ready, impossible, reason := dependencyState(job, jobsByID)
			if impossible {
				if _, failErr := coordinator.repository.FailQueuedJob(ctx, job.ID, reason, coordinator.now().UTC()); failErr != nil && !errors.Is(failErr, ErrConflict) {
					return failErr
				}
				continue
			}
			if !ready {
				if err := coordinator.recordJobBlock(ctx, job, blockReason("dependency_wait", "dependency", "waiting for declared job dependencies", map[string]string{"dependencyCount": strconv.Itoa(len(job.Dependencies))})); err != nil {
					return err
				}
				continue
			}
			pending = append(pending, job)
		}
	}
	blockedQueues := make(map[string]bool)
	var liveTopology *domain.TopologySnapshot
	if coordinator.topology != nil {
		if snapshot, topologyErr := coordinator.topology.LatestSnapshot(ctx); topologyErr == nil {
			liveTopology = &snapshot
		}
	}
	for len(pending) > 0 {
		sort.SliceStable(pending, func(i, j int) bool { return fairJobLess(pending[i], pending[j], stats, policyByQueue) })
		job := pending[0]
		pending = pending[1:]
		policy := effectiveQueuePolicy(job.Queue, policyByQueue)
		if blockedQueues[job.Queue] {
			if err := coordinator.recordJobBlock(ctx, job, blockReason("queue_head_blocked", "queue", "an earlier job is blocking this non-backfill queue", map[string]string{"queue": job.Queue})); err != nil {
				return err
			}
			continue
		}
		if !policyAllowsJob(policy, job, stats) {
			if err := coordinator.recordJobBlock(ctx, job, policyBlockReason(policy, job, stats)); err != nil {
				return err
			}
			continue
		}
		if len(job.PreemptionVictims) > 0 {
			if err := coordinator.recordJobBlock(ctx, job, blockReason("preemption_wait", "scheduler", "waiting for preempted assignments to stop", map[string]string{"victimCount": strconv.Itoa(len(job.PreemptionVictims))})); err != nil {
				return err
			}
			continue
		}
		nodes, err := coordinator.nodes.ListNodes(ctx)
		if err != nil {
			return err
		}
		nodes = reservationEligibleNodes(job, nodes, policyByQueue, coordinator.now().UTC())
		parallel := job.Task.Parallel.Effective()
		if parallel.Nodes > 1 {
			selection := selectNodeSetWithTopologyDetailed(job, nodes, int(parallel.Nodes), liveTopology, 100_000)
			if len(selection.nodes) == 0 {
				reason := nodeBlockReason(job, nodes, liveTopology)
				if selection.budgetExhausted {
					reason = blockReason("gang_search_budget", "scheduler", "gang search budget was exhausted before a feasible set was found", map[string]string{
						"searchStates": strconv.Itoa(selection.visited), "searchLimit": "100000",
					})
				}
				if err := coordinator.recordJobBlock(ctx, job, reason); err != nil {
					return err
				}
				if !policy.Backfill {
					blockedQueues[job.Queue] = true
				}
				continue
			}
			allocation, allocationErr := coordinator.newAllocation(job, selection.nodes, liveTopology)
			if allocationErr != nil {
				return allocationErr
			}
			allocation.SelectionOptimal = !selection.budgetExhausted
			allocation.SelectionSearchStates = selection.visited
			repository, supported := coordinator.repository.(gangRepository)
			queue, batchSupported := coordinator.commands.(batchCommandQueue)
			if !supported || !batchSupported {
				return errors.New("multi-node scheduling requires transactional allocation and atomic command queue support")
			}
			assigned, assignErr := repository.AssignJobAllocation(ctx, job.ID, allocation, coordinator.now().UTC())
			if errors.Is(assignErr, ErrConflict) || errors.Is(assignErr, ErrNoCapacity) {
				continue
			}
			if assignErr != nil {
				return assignErr
			}
			commands := make(map[string][]*xirangv1.ControlMessage, len(assigned.Allocation.Members))
			for _, member := range assigned.Allocation.Members {
				commands[member.NodeID] = append(commands[member.NodeID], allocationCommand(assigned, member))
			}
			if enqueueErr := queue.EnqueueBatch(commands); enqueueErr != nil {
				if _, rollbackErr := repository.RollbackJobAllocation(ctx, assigned.ID, allocation.ID, enqueueErr.Error(), coordinator.now().UTC()); rollbackErr != nil {
					return errors.Join(enqueueErr, rollbackErr)
				}
				return enqueueErr
			}
			stats.add(assigned)
			continue
		}
		node, ok := domain.Node{}, false
		if job.PreemptionNodeID != "" {
			for _, candidate := range nodes {
				if candidate.ID == job.PreemptionNodeID && NodeFitsJob(candidate, job) && topologyFitsJob(candidate, job, liveTopology) {
					node, ok = candidate, true
					break
				}
			}
		}
		if !ok {
			node, ok = selectNodeWithTopology(job, nodes, liveTopology)
		}
		if !ok {
			if policy.AllowPreemption {
				node, victims, found := selectPreemptionPlan(job, jobs, nodes, liveTopology, policyByQueue)
				if found {
					grace := time.Duration(policy.PreemptionGraceSeconds) * time.Second
					if grace == 0 {
						grace = 10 * time.Second
					}
					reason := fmt.Sprintf("preempted by higher-priority job %s (priority %d)", job.ID, job.Priority)
					requested, requestErr := coordinator.repository.RequestJobPreemption(
						ctx, job.ID, node.ID, victimIDs(victims), reason, grace, coordinator.now().UTC(),
					)
					if errors.Is(requestErr, ErrConflict) {
						continue
					}
					if requestErr != nil {
						return requestErr
					}
					for _, victim := range requested {
						if err := coordinator.commands.Enqueue(victim.NodeID, cancellationCommand(victim)); err != nil {
							return err
						}
					}
					continue
				}
			}
			if !policy.Backfill {
				blockedQueues[job.Queue] = true
			}
			if err := coordinator.recordJobBlock(ctx, job, nodeBlockReason(job, nodes, liveTopology)); err != nil {
				return err
			}
			continue
		}
		assignmentID, err := randomID(coordinator.random, "assignment_")
		if err != nil {
			return err
		}
		assigned, err := coordinator.repository.AssignJob(ctx, job.ID, node.ID, assignmentID, coordinator.now().UTC())
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNoCapacity) {
			continue
		}
		if err != nil {
			return err
		}
		message := assignmentCommand(assigned, node.ID)
		if err := coordinator.commands.Enqueue(node.ID, message); err != nil {
			if _, rollbackErr := coordinator.repository.RollbackAssignment(ctx, assigned.ID, assigned.AssignmentID, err.Error(), coordinator.now().UTC()); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
		stats.add(assigned)
	}
	return nil
}

// defaultFairShareHalfLife applies to a queue policy that enables historical
// usage weighting without naming its own half-life.
const defaultFairShareHalfLife = 7 * 24 * time.Hour

// fairShareHorizon is how far back completed work still measurably influences
// fair-share ordering: ten half-lives, after which a job's remaining weight is
// under a thousandth of what it started with.
func fairShareHorizon(policies []domain.QueuePolicy) time.Duration {
	halfLife := defaultFairShareHalfLife
	for _, policy := range policies {
		if configured := time.Duration(policy.FairShareHalfLifeHours) * time.Hour; configured > halfLife {
			halfLife = configured
		}
	}
	return 10 * halfLife
}

// resolveDeclaredDependencies fills in prerequisites that the scheduling window
// did not already contain. A prerequisite that genuinely no longer exists is
// left absent so dependencyState can report it.
func (coordinator *Coordinator) resolveDeclaredDependencies(ctx context.Context, jobs []domain.Job, jobsByID map[string]domain.Job) error {
	for _, job := range jobs {
		if job.Status != domain.JobStatusQueued {
			continue
		}
		for _, dependency := range job.Dependencies {
			if _, known := jobsByID[dependency.JobID]; known {
				continue
			}
			prerequisite, err := coordinator.repository.GetJob(ctx, dependency.JobID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			jobsByID[dependency.JobID] = prerequisite
		}
	}
	return nil
}

func dependencyState(job domain.Job, jobs map[string]domain.Job) (bool, bool, string) {
	for _, dependency := range job.Dependencies {
		prerequisite, ok := jobs[dependency.JobID]
		if !ok {
			return false, true, "dependency disappeared: " + dependency.JobID
		}
		switch dependency.Condition {
		case "afterany":
			if !prerequisite.Status.Terminal() {
				return false, false, ""
			}
		case "afterok":
			if prerequisite.Status == domain.JobStatusSucceeded {
				continue
			}
			if prerequisite.Status.Terminal() {
				return false, true, "dependency did not succeed: " + dependency.JobID
			}
			return false, false, ""
		default:
			return false, true, "dependency condition is invalid"
		}
	}
	return true, false, ""
}

// Elasticity returns bounded capacity advice. It deliberately has no
// provisioning side effect and reports only queues with an explicit budget.
func (coordinator *Coordinator) Elasticity(ctx context.Context) (domain.ElasticitySnapshot, error) {
	snapshot := domain.ElasticitySnapshot{GeneratedAt: coordinator.now().UTC(), Recommendations: []domain.ElasticityRecommendation{}}
	// Only queued demand is measured, so the live working set is enough.
	jobs, err := coordinator.repository.ListActiveJobs(ctx)
	if err != nil {
		return snapshot, err
	}
	nodes, err := coordinator.nodes.ListNodes(ctx)
	if err != nil {
		return snapshot, err
	}
	policies, err := coordinator.repository.ListQueuePolicies(ctx)
	if err != nil {
		return snapshot, err
	}
	policyByQueue := make(map[string]domain.QueuePolicy, len(policies))
	for _, policy := range policies {
		policyByQueue[policy.Name] = policy
	}
	type demand struct {
		count     int32
		resources domain.ResourceVector
	}
	byQueue := map[string]demand{}
	for _, job := range jobs {
		if job.Status != domain.JobStatusQueued {
			continue
		}
		current := byQueue[job.Queue]
		current.count++
		current.resources = current.resources.Add(jobReservedResources(job))
		byQueue[job.Queue] = current
	}
	for queue, pending := range byQueue {
		policy := effectiveQueuePolicy(queue, policyByQueue)
		if policy.MaxRecommendedScaleOut <= 0 || resourceVectorEmpty(policy.ElasticityBudget) {
			continue
		}
		var free, largest domain.ResourceVector
		for _, node := range nodes {
			if !node.Schedulable() {
				continue
			}
			available, subtractErr := node.Allocatable.Subtract(node.Allocated)
			if subtractErr != nil {
				continue
			}
			free = free.Add(available)
			largest = componentMax(largest, node.Allocatable)
		}
		deficit := positiveDeficit(pending.resources, free)
		if resourceVectorEmpty(deficit) {
			continue
		}
		suggested := nodesForDeficit(deficit, largest)
		limited := false
		budgetNodes := nodesForBudget(policy.ElasticityBudget, largest)
		if budgetNodes < suggested {
			suggested, limited = budgetNodes, true
		}
		if suggested > policy.MaxRecommendedScaleOut {
			suggested, limited = policy.MaxRecommendedScaleOut, true
		}
		if suggested < 0 {
			suggested = 0
		}
		reason := "queued demand exceeds currently free schedulable capacity"
		if suggested == 0 {
			reason = "capacity deficit detected, but the configured elasticity budget cannot fund a representative node"
		}
		snapshot.Recommendations = append(snapshot.Recommendations, domain.ElasticityRecommendation{
			Queue: queue, QueuedJobs: pending.count, QueuedResources: pending.resources,
			CapacityDeficit: deficit, SuggestedAdditionalNodes: suggested,
			LimitedByBudget: limited, Reason: reason,
		})
	}
	sort.Slice(snapshot.Recommendations, func(i, j int) bool {
		return snapshot.Recommendations[i].Queue < snapshot.Recommendations[j].Queue
	})
	return snapshot, nil
}

type schedulingStats struct {
	queueCount   map[string]int32
	projectCount map[string]int32
	queueUsage   map[string]domain.ResourceVector
	projectUsage map[string]domain.ResourceVector
	ownerDebt    map[string]float64
}

func buildSchedulingStats(jobs []domain.Job, policies map[string]domain.QueuePolicy, now time.Time) *schedulingStats {
	stats := &schedulingStats{
		queueCount: make(map[string]int32), projectCount: make(map[string]int32),
		queueUsage: make(map[string]domain.ResourceVector), projectUsage: make(map[string]domain.ResourceVector),
		ownerDebt: make(map[string]float64),
	}
	for _, job := range jobs {
		policy := effectiveQueuePolicy(job.Queue, policies)
		if job.Status.Terminal() && job.FinishedAt != nil && job.OwnerID != "" && policy.HistoricalUsageWeight > 0 {
			halfLife := time.Duration(policy.FairShareHalfLifeHours) * time.Hour
			if halfLife <= 0 {
				halfLife = defaultFairShareHalfLife
			}
			age := now.Sub(*job.FinishedAt)
			if age < 0 {
				age = 0
			}
			decay := math.Exp2(-float64(age) / float64(halfLife))
			coreHours := float64(job.Usage.CPUUsageMicros) / 3_600_000_000
			gpuHours := float64(job.Usage.GPUDeviceSeconds) / 3600
			stats.ownerDebt[job.OwnerID] += (coreHours + gpuHours*8) * decay * float64(policy.HistoricalUsageWeight)
		}
		switch job.Status {
		case domain.JobStatusAssigned, domain.JobStatusRunning, domain.JobStatusSuspended, domain.JobStatusCanceling, domain.JobStatusOrphaned:
			stats.add(job)
		}
	}
	return stats
}

func (stats *schedulingStats) add(job domain.Job) {
	projectKey := job.Queue + "\x00" + job.ProjectID
	stats.queueCount[job.Queue]++
	stats.projectCount[projectKey]++
	resources := jobReservedResources(job)
	stats.queueUsage[job.Queue] = stats.queueUsage[job.Queue].Add(resources)
	stats.projectUsage[projectKey] = stats.projectUsage[projectKey].Add(resources)
}

func effectiveQueuePolicy(name string, policies map[string]domain.QueuePolicy) domain.QueuePolicy {
	if policy, ok := policies[name]; ok {
		if policy.Weight <= 0 {
			policy.Weight = 1
		}
		return policy
	}
	return domain.QueuePolicy{Name: name, Weight: 1, Backfill: true}
}

func fairJobLess(left, right domain.Job, stats *schedulingStats, policies map[string]domain.QueuePolicy) bool {
	leftReady := left.PreemptionNodeID != "" && len(left.PreemptionVictims) == 0
	rightReady := right.PreemptionNodeID != "" && len(right.PreemptionVictims) == 0
	if leftReady != rightReady {
		return leftReady
	}
	leftPolicy, rightPolicy := effectiveQueuePolicy(left.Queue, policies), effectiveQueuePolicy(right.Queue, policies)
	if leftQoS, rightQoS := qosRank(leftPolicy.QoS), qosRank(rightPolicy.QoS); leftQoS != rightQoS {
		return leftQoS > rightQoS
	}
	leftDebt := stats.ownerDebt[left.OwnerID] / float64(maxInt32(leftPolicy.Weight, 1))
	rightDebt := stats.ownerDebt[right.OwnerID] / float64(maxInt32(rightPolicy.Weight, 1))
	if math.Abs(leftDebt-rightDebt) > 1e-9 {
		return leftDebt < rightDebt
	}
	leftShare := int64(stats.queueCount[left.Queue]) * int64(rightPolicy.Weight)
	rightShare := int64(stats.queueCount[right.Queue]) * int64(leftPolicy.Weight)
	if leftShare != rightShare {
		return leftShare < rightShare
	}
	leftProject := stats.projectCount[left.Queue+"\x00"+left.ProjectID]
	rightProject := stats.projectCount[right.Queue+"\x00"+right.ProjectID]
	if leftProject != rightProject {
		return leftProject < rightProject
	}
	if left.Priority != right.Priority {
		return left.Priority > right.Priority
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	return left.ID < right.ID
}

func policyAllowsJob(policy domain.QueuePolicy, job domain.Job, stats *schedulingStats) bool {
	projectKey := job.Queue + "\x00" + job.ProjectID
	if policy.MaxConcurrent > 0 && stats.queueCount[job.Queue] >= policy.MaxConcurrent {
		return false
	}
	if policy.MaxConcurrentPerProject > 0 && stats.projectCount[projectKey] >= policy.MaxConcurrentPerProject {
		return false
	}
	resources := jobReservedResources(job)
	return quotaFits(policy.ResourceQuota, stats.queueUsage[job.Queue].Add(resources)) &&
		quotaFits(policy.ProjectResourceQuota, stats.projectUsage[projectKey].Add(resources))
}

func (coordinator *Coordinator) recordJobBlock(ctx context.Context, job domain.Job, reason *domain.JobBlockReason) error {
	repository, ok := coordinator.repository.(queuedBlockRepository)
	if !ok {
		return nil
	}
	_, err := repository.UpdateQueuedJobBlock(ctx, job.ID, reason, coordinator.now().UTC())
	if errors.Is(err, ErrConflict) {
		return nil
	}
	return err
}

func blockReason(code, category, message string, details map[string]string) *domain.JobBlockReason {
	return &domain.JobBlockReason{Code: code, Category: category, Message: message, Details: details}
}

func policyBlockReason(policy domain.QueuePolicy, job domain.Job, stats *schedulingStats) *domain.JobBlockReason {
	projectKey := job.Queue + "\x00" + job.ProjectID
	details := map[string]string{"queue": job.Queue, "project": job.ProjectID}
	if policy.MaxConcurrent > 0 && stats.queueCount[job.Queue] >= policy.MaxConcurrent {
		details["limit"] = strconv.FormatInt(int64(policy.MaxConcurrent), 10)
		details["current"] = strconv.FormatInt(int64(stats.queueCount[job.Queue]), 10)
		return blockReason("queue_concurrency_limit", "quota", "queue concurrency limit is in use", details)
	}
	if policy.MaxConcurrentPerProject > 0 && stats.projectCount[projectKey] >= policy.MaxConcurrentPerProject {
		details["limit"] = strconv.FormatInt(int64(policy.MaxConcurrentPerProject), 10)
		details["current"] = strconv.FormatInt(int64(stats.projectCount[projectKey]), 10)
		return blockReason("project_concurrency_limit", "quota", "project concurrency limit is in use", details)
	}
	resources := jobReservedResources(job)
	if !quotaFits(policy.ResourceQuota, stats.queueUsage[job.Queue].Add(resources)) {
		return blockReason("queue_resource_quota", "quota", "queue resource quota has insufficient headroom", resourceDetails(resources, details))
	}
	return blockReason("project_resource_quota", "quota", "project resource quota has insufficient headroom", resourceDetails(resources, details))
}

func nodeBlockReason(job domain.Job, nodes []domain.Node, topology *domain.TopologySnapshot) *domain.JobBlockReason {
	details := resourceDetails(job.Task.Resources, map[string]string{
		"nodesRequested": strconv.FormatInt(int64(job.Task.Parallel.Effective().Nodes), 10),
		"nodesObserved":  strconv.Itoa(len(nodes)),
	})
	if len(nodes) == 0 {
		return blockReason("no_nodes", "node", "no enrolled nodes are available", details)
	}
	schedulable := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Schedulable() && containsText(node.Capabilities, "cgroup-v2-executor-v1") {
			schedulable = append(schedulable, node)
		}
	}
	details["schedulableNodes"] = strconv.Itoa(len(schedulable))
	if len(schedulable) == 0 {
		return blockReason("no_ready_compute_node", "node", "no ready compute node with the native executor is available", details)
	}
	requiredArchitecture := job.Task.Placement.RequiredArchitecture
	if requiredArchitecture == "" {
		requiredArchitecture = job.Task.Artifact.Architecture
	}
	architectureNodes := make([]domain.Node, 0, len(schedulable))
	for _, node := range schedulable {
		if requiredArchitecture == "" || node.Architecture == requiredArchitecture {
			architectureNodes = append(architectureNodes, node)
		}
	}
	details["requiredArchitecture"] = requiredArchitecture
	details["architectureNodes"] = strconv.Itoa(len(architectureNodes))
	if len(architectureNodes) == 0 {
		return blockReason("architecture_unavailable", "architecture", "no schedulable node matches the required architecture", details)
	}
	parallel := job.Task.Parallel.Effective()
	if parallel.Nodes > 1 && parallel.Launcher != "native" {
		capability := "mpi-launch-v1/" + parallel.Launcher
		count := 0
		for _, node := range architectureNodes {
			if containsText(node.Capabilities, capability) {
				count++
			}
		}
		details["requiredCapability"] = capability
		details["capableNodes"] = strconv.Itoa(count)
		if count < int(parallel.Nodes) {
			return blockReason("mpi_adapter_unavailable", "capability", "too few nodes have a certified MPI launcher", details)
		}
	}
	for resource, quantity := range job.Task.Resources.Devices {
		capability := "device-resource-executor-v1/" + resource
		count := 0
		for _, node := range architectureNodes {
			if containsText(node.Capabilities, capability) && node.Allocatable.Devices[resource]-node.Allocated.Devices[resource] >= quantity {
				count++
			}
		}
		details["device"] = resource
		details["deviceQuantity"] = strconv.FormatInt(quantity, 10)
		details["deviceCapableNodes"] = strconv.Itoa(count)
		if count < int(parallel.Nodes) {
			return blockReason("device_capacity_unavailable", "device", "requested accelerator capacity is not available", details)
		}
	}
	staticMatches, resourceMatches := 0, 0
	for _, node := range architectureNodes {
		if !nodeMatchesJob(node, job) {
			continue
		}
		staticMatches++
		available, err := node.Allocatable.Subtract(node.Allocated)
		if err == nil && available.Fits(job.Task.Resources) {
			resourceMatches++
		}
	}
	details["placementMatches"] = strconv.Itoa(staticMatches)
	details["resourceMatches"] = strconv.Itoa(resourceMatches)
	if staticMatches == 0 {
		return blockReason("placement_constraints", "placement", "node labels, taints, exclusions, or affinity constraints do not match", details)
	}
	if resourceMatches < int(parallel.Nodes) {
		return blockReason("resource_capacity", "resource", "insufficient free CPU, memory, or requested device capacity", details)
	}
	if parallel.Nodes > 1 && job.Task.Placement.MinimumNetworkBits > 0 {
		details["minimumNetworkBits"] = strconv.FormatInt(job.Task.Placement.MinimumNetworkBits, 10)
		if topology == nil {
			return blockReason("topology_unavailable", "topology", "network topology is unavailable for this multi-node request", details)
		}
		return blockReason("topology_capacity", "topology", "no connected node set meets the requested network capacity", details)
	}
	if parallel.Nodes > 1 {
		return blockReason("gang_capacity", "resource", "no simultaneous node set is available for the gang allocation", details)
	}
	return blockReason("resource_capacity", "resource", "no node currently satisfies all requested resources", details)
}

func resourceDetails(resources domain.ResourceVector, details map[string]string) map[string]string {
	if details == nil {
		details = map[string]string{}
	}
	details["requestedMilliCPU"] = strconv.FormatInt(resources.MilliCPU, 10)
	details["requestedMemoryBytes"] = strconv.FormatInt(resources.MemoryBytes, 10)
	for name, quantity := range resources.Devices {
		details["device."+name] = strconv.FormatInt(quantity, 10)
	}
	return details
}

func jobReservedResources(job domain.Job) domain.ResourceVector {
	nodes := int64(job.Task.Parallel.Effective().Nodes)
	result := domain.ResourceVector{
		MilliCPU: job.Task.Resources.MilliCPU * nodes, MemoryBytes: job.Task.Resources.MemoryBytes * nodes,
		EphemeralStorageBytes: job.Task.Resources.EphemeralStorageBytes * nodes, Devices: make(map[string]int64),
	}
	for name, quantity := range job.Task.Resources.Devices {
		result.Devices[name] = quantity * nodes
	}
	return result
}

func qosRank(value string) int {
	return map[string]int{"normal": 1, "high": 2, "urgent": 3}[value]
}

func maxInt32(value, floor int32) int32 {
	if value < floor {
		return floor
	}
	return value
}

func reservationEligibleNodes(job domain.Job, nodes []domain.Node, policies map[string]domain.QueuePolicy, now time.Time) []domain.Node {
	result := make([]domain.Node, 0, len(nodes))
	jobPolicy := effectiveQueuePolicy(job.Queue, policies)
	jobReserved := reservationActive(jobPolicy, now)
	for _, node := range nodes {
		reservedForAnotherQueue := false
		for queue, policy := range policies {
			if queue == job.Queue || !reservationActive(policy, now) {
				continue
			}
			if reservationContainsNode(policy, node.ID) {
				reservedForAnotherQueue = true
				break
			}
		}
		if reservedForAnotherQueue {
			continue
		}
		if jobReserved {
			if len(jobPolicy.ReservationGroupIDs) > 0 && !containsString(jobPolicy.ReservationGroupIDs, job.GroupID) {
				continue
			}
			if len(jobPolicy.ReservationNodeIDs) > 0 && !reservationContainsNode(jobPolicy, node.ID) {
				continue
			}
		}
		result = append(result, node)
	}
	return result
}

func reservationActive(policy domain.QueuePolicy, now time.Time) bool {
	return policy.ReservationStart != nil && policy.ReservationEnd != nil &&
		!now.Before(*policy.ReservationStart) && now.Before(*policy.ReservationEnd)
}

func reservationContainsNode(policy domain.QueuePolicy, nodeID string) bool {
	return len(policy.ReservationNodeIDs) > 0 && containsString(policy.ReservationNodeIDs, nodeID)
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func quotaFits(quota, requested domain.ResourceVector) bool {
	if quota.MilliCPU > 0 && requested.MilliCPU > quota.MilliCPU ||
		quota.MemoryBytes > 0 && requested.MemoryBytes > quota.MemoryBytes ||
		quota.EphemeralStorageBytes > 0 && requested.EphemeralStorageBytes > quota.EphemeralStorageBytes {
		return false
	}
	for name, quantity := range quota.Devices {
		if quantity > 0 && requested.Devices[name] > quantity {
			return false
		}
	}
	return true
}

// RestoreAssignments reconstructs accepted-but-not-yet-reported Run commands.
// Replaying the same assignment/attempt is safe because the Agent hashes its spec.
func (coordinator *Coordinator) RestoreAssignments(ctx context.Context) error {
	// Only work that is still in flight can need replaying, and a finished job
	// must never have its assignment re-sent.
	jobs, err := coordinator.repository.ListActiveJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.AssignmentID == "" || job.NodeID == "" {
			continue
		}
		if job.Allocation != nil {
			queue, ok := coordinator.commands.(batchCommandQueue)
			if !ok {
				return errors.New("restoring a gang allocation requires atomic command queue support")
			}
			commands := make(map[string][]*xirangv1.ControlMessage)
			switch {
			case job.Status == domain.JobStatusCanceling || job.Allocation.State == domain.AllocationStateCanceling:
				for _, member := range job.Allocation.Members {
					if !member.State.Terminal() {
						commands[member.NodeID] = append(commands[member.NodeID], allocationCancellationCommand(job, member))
					}
				}
			case job.Allocation.State == domain.AllocationStateReady:
				for _, member := range job.Allocation.Members {
					commands[member.NodeID] = append(commands[member.NodeID], allocationReleaseCommand(job, member))
				}
			case job.Allocation.State == domain.AllocationStatePreparing:
				for _, member := range job.Allocation.Members {
					if member.State != domain.AssignmentStateReady {
						commands[member.NodeID] = append(commands[member.NodeID], allocationCommand(job, member))
					}
				}
			}
			if len(commands) > 0 {
				if err := queue.EnqueueBatch(commands); err != nil {
					return err
				}
			}
			continue
		}
		var command *xirangv1.ControlMessage
		switch job.Status {
		case domain.JobStatusAssigned:
			command = assignmentCommand(job, job.NodeID)
		case domain.JobStatusCanceling:
			command = cancellationCommand(job)
		default:
			continue
		}
		if err := coordinator.commands.Enqueue(job.NodeID, command); err != nil {
			return err
		}
	}
	return nil
}

func (coordinator *Coordinator) ConfirmAssignmentCancellation(ctx context.Context, nodeID string, command *xirangv1.CancelAssignment, accepted bool, reason string) error {
	if command == nil || !validText(nodeID, 256, false) || !validText(command.AssignmentId, 256, false) {
		return ErrInvalidJob
	}
	if accepted {
		return nil
	}
	_, err := coordinator.repository.RejectJobCancellation(ctx, command.AssignmentId, boundedReason(reason), coordinator.now().UTC())
	coordinator.notify()
	return err
}

func (coordinator *Coordinator) ConfirmAssignmentCommand(ctx context.Context, nodeID string, command *xirangv1.RunAssignment, accepted bool, reason string) error {
	if command == nil || command.AssignmentId == "" || command.JobId == "" || command.Attempt == 0 {
		return ErrInvalidJob
	}
	if accepted {
		return nil
	}
	job, err := coordinator.repository.RejectAssignment(ctx, command.JobId, command.AssignmentId, boundedReason(reason), coordinator.now().UTC())
	if err == nil && job.Allocation != nil {
		err = coordinator.dispatchAllocationTransition(job)
	}
	coordinator.notify()
	return err
}

func (coordinator *Coordinator) ConfirmAssignmentRelease(ctx context.Context, nodeID string, command *xirangv1.ReleaseAssignment, accepted bool, reason string) error {
	if command == nil || !validText(nodeID, 256, false) || !validText(command.AssignmentId, 256, false) ||
		!validText(command.AllocationId, 256, false) || !validText(command.JobId, 256, false) || command.Attempt == 0 {
		return ErrInvalidJob
	}
	if accepted {
		return nil
	}
	return coordinator.ReportAssignment(ctx, nodeID, &xirangv1.AssignmentUpdate{
		AssignmentId: command.AssignmentId, Attempt: command.Attempt,
		State:  xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED,
		Reason: "gang release rejected: " + boundedReason(reason), OccurredAt: timestamppb.New(coordinator.now().UTC()),
	})
}

func (coordinator *Coordinator) ReportAssignment(ctx context.Context, nodeID string, update *xirangv1.AssignmentUpdate) error {
	if update == nil || !validText(nodeID, 256, false) || !validText(update.AssignmentId, 256, false) || update.Attempt == 0 || update.OccurredAt == nil || len(update.Reason) > 4096 || len(update.StdoutTail) > 64<<10 || len(update.StderrTail) > 64<<10 {
		return ErrInvalidJob
	}
	if err := update.OccurredAt.CheckValid(); err != nil {
		return ErrInvalidJob
	}
	state, ok := protocolAssignmentState(update.State)
	if !ok {
		return ErrInvalidJob
	}
	now := coordinator.now().UTC()
	occurredAt := update.OccurredAt.AsTime().UTC()
	if occurredAt.After(now.Add(5 * time.Minute)) {
		return ErrInvalidJob
	}
	job, changed, err := coordinator.repository.ApplyAssignmentReport(ctx, domain.AssignmentReport{
		NodeID: nodeID, AssignmentID: update.AssignmentId, Attempt: update.Attempt,
		State: state, ExitCode: update.ExitCode, Reason: boundedReason(update.Reason),
		StdoutTail: string(update.StdoutTail), StderrTail: string(update.StderrTail), LogsTruncated: update.LogsTruncated,
		OccurredAt: occurredAt, ReceivedAt: now,
	})
	if changed {
		if job.Allocation != nil {
			if dispatchErr := coordinator.dispatchAllocationTransition(job); dispatchErr != nil && err == nil {
				err = dispatchErr
			}
		}
		if coordinator.alerts != nil {
			fingerprint := "job_storage_" + job.ID
			if state == domain.AssignmentStateSuspended {
				_, alertErr := coordinator.alerts.ReportCondition(ctx, fingerprint, "job_storage", nodeID, domain.AlertSeverityCritical, job.Reason)
				if err == nil && alertErr != nil {
					err = alertErr
				}
			} else if state == domain.AssignmentStateRunning || state.Terminal() {
				// A missing condition is normal for jobs that never suspended.
				_, _ = coordinator.alerts.ResolveCondition(ctx, fingerprint, "job storage condition recovered")
			}
		}
		coordinator.notify()
	}
	return err
}

func (coordinator *Coordinator) ReportAssignmentLog(ctx context.Context, nodeID string, snapshot *xirangv1.AssignmentLogSnapshot) error {
	if snapshot == nil || !validText(nodeID, 256, false) || !validText(snapshot.AssignmentId, 256, false) || snapshot.Attempt == 0 ||
		snapshot.ObservedAt == nil || len(snapshot.StdoutTail) > 64<<10 || len(snapshot.StderrTail) > 64<<10 {
		return ErrInvalidJob
	}
	if err := snapshot.ObservedAt.CheckValid(); err != nil {
		return ErrInvalidJob
	}
	now := coordinator.now().UTC()
	observedAt := snapshot.ObservedAt.AsTime().UTC()
	if observedAt.After(now.Add(5 * time.Minute)) {
		return ErrInvalidJob
	}
	_, changed, err := coordinator.repository.ApplyAssignmentLog(ctx, domain.AssignmentLogSnapshot{
		NodeID: nodeID, AssignmentID: snapshot.AssignmentId, Attempt: snapshot.Attempt,
		StdoutTail: string(snapshot.StdoutTail), StderrTail: string(snapshot.StderrTail), LogsTruncated: snapshot.LogsTruncated,
		ObservedAt: observedAt, ReceivedAt: now,
	})
	if changed {
		coordinator.notify()
	}
	return err
}

func (coordinator *Coordinator) ReportAssignmentUsage(ctx context.Context, nodeID string, snapshot *xirangv1.AssignmentUsageSnapshot) error {
	if snapshot == nil || !validText(nodeID, 256, false) || !validText(snapshot.AssignmentId, 256, false) ||
		snapshot.Attempt == 0 || snapshot.ObservedAt == nil {
		return ErrInvalidJob
	}
	if err := snapshot.ObservedAt.CheckValid(); err != nil {
		return ErrInvalidJob
	}
	now := coordinator.now().UTC()
	observedAt := snapshot.ObservedAt.AsTime().UTC()
	if observedAt.After(now.Add(5 * time.Minute)) {
		return ErrInvalidJob
	}
	job, changed, err := coordinator.repository.ApplyAssignmentUsage(ctx, domain.AssignmentUsageSnapshot{
		NodeID: nodeID, AssignmentID: snapshot.AssignmentId, Attempt: snapshot.Attempt,
		CPUUsageMicros: snapshot.CpuUsageMicros, MemoryByteSeconds: snapshot.MemoryByteSeconds,
		GPUDeviceSeconds: snapshot.GpuDeviceSeconds, WallTimeMillis: snapshot.WallTimeMillis,
		WorkspaceBytes: snapshot.WorkspaceBytes, FilesystemAvailableBytes: snapshot.FilesystemAvailableBytes,
		FilesystemTotalBytes: snapshot.FilesystemTotalBytes, ObservedAt: observedAt, ReceivedAt: now,
	})
	if changed {
		if quotaErr := coordinator.reconcileGroupStorage(ctx, job.GroupID); err == nil {
			err = quotaErr
		}
		if coordinator.alerts != nil && job.Usage.FilesystemTotalBytes > 0 {
			reserve := controllerStorageReserve(job.Usage.FilesystemTotalBytes)
			fingerprint := "node_storage_" + nodeID
			switch {
			case job.Usage.FilesystemAvailableBytes < reserve:
				_, _ = coordinator.alerts.ReportCondition(ctx, fingerprint, "node_storage", nodeID, domain.AlertSeverityCritical,
					fmt.Sprintf("node storage safety reserve exhausted: %d bytes available, %d bytes required", job.Usage.FilesystemAvailableBytes, reserve))
			case job.Usage.FilesystemAvailableBytes < saturatingUint64Add(reserve, reserve):
				_, _ = coordinator.alerts.ReportCondition(ctx, fingerprint, "node_storage", nodeID, domain.AlertSeverityWarning,
					fmt.Sprintf("node storage is approaching the safety reserve: %d bytes available", job.Usage.FilesystemAvailableBytes))
			default:
				_, _ = coordinator.alerts.ResolveCondition(ctx, fingerprint, "node storage capacity recovered")
			}
		}
		coordinator.notify()
	}
	return err
}

func (coordinator *Coordinator) UpdateGroupStorageQuota(ctx context.Context, groupID string, quotaBytes int64) error {
	if !validText(strings.TrimSpace(groupID), 256, false) || quotaBytes < 0 {
		return ErrInvalidJob
	}
	repository, ok := coordinator.repository.(storageQuotaRepository)
	if !ok {
		return errors.New("storage quota persistence is unavailable")
	}
	if _, err := repository.UpdateGroupJobStorageQuota(ctx, strings.TrimSpace(groupID), quotaBytes, coordinator.now().UTC()); err != nil {
		return err
	}
	return coordinator.reconcileGroupStorage(ctx, strings.TrimSpace(groupID))
}

func (coordinator *Coordinator) UpdateUserStorageQuota(ctx context.Context, ownerID string, quotaBytes int64) error {
	ownerID = strings.TrimSpace(ownerID)
	if !validText(ownerID, 256, false) || quotaBytes < 0 {
		return ErrInvalidJob
	}
	repository, ok := coordinator.repository.(userStorageQuotaRepository)
	if !ok {
		return errors.New("user storage quota persistence is unavailable")
	}
	_, err := repository.UpdateUserJobStorageQuota(ctx, ownerID, quotaBytes, coordinator.now().UTC())
	return err
}

func (coordinator *Coordinator) SetGroupDisabled(ctx context.Context, groupID string, disabled bool) error {
	groupID = strings.TrimSpace(groupID)
	if !validText(groupID, 256, false) {
		return ErrInvalidJob
	}
	repository, ok := coordinator.repository.(groupDisableRepository)
	if !ok {
		return errors.New("group job policy persistence is unavailable")
	}
	reason := "user group disabled by administrator"
	if _, err := repository.SetGroupJobsSuspended(ctx, groupID, disabled, reason, coordinator.now().UTC()); err != nil {
		return err
	}
	coordinator.notify()
	return nil
}

func (coordinator *Coordinator) reconcileGroupStorage(ctx context.Context, groupID string) error {
	if groupID == "" {
		return nil
	}
	// Finished jobs still count towards the group's workspace total below, so
	// this needs the full history rather than the live working set.
	jobs, err := coordinator.repository.ListAllJobs(ctx)
	if err != nil {
		return err
	}
	active := make([]domain.Job, 0)
	var total uint64
	var quota uint64
	for _, job := range jobs {
		if job.GroupID != groupID {
			continue
		}
		total = saturatingUint64Add(total, job.Usage.WorkspaceBytes)
		if job.StorageQuotaBytes > 0 {
			quota = uint64(job.StorageQuotaBytes)
		}
		if job.Status.Terminal() || job.AssignmentID == "" || job.NodeID == "" {
			continue
		}
		active = append(active, job)
	}
	exceeded := quota > 0 && total > quota
	if coordinator.alerts != nil && quota > 0 {
		fingerprint := "group_storage_" + groupID
		switch {
		case exceeded:
			_, _ = coordinator.alerts.ReportCondition(ctx, fingerprint, "group_storage", "", domain.AlertSeverityCritical,
				fmt.Sprintf("user group %s storage quota exceeded: %d of %d bytes used", groupID, total, quota))
		case total >= quota-quota/5:
			_, _ = coordinator.alerts.ReportCondition(ctx, fingerprint, "group_storage", "", domain.AlertSeverityWarning,
				fmt.Sprintf("user group %s storage usage reached %d of %d bytes", groupID, total, quota))
		default:
			_, _ = coordinator.alerts.ResolveCondition(ctx, fingerprint, "user group storage usage returned below the warning threshold")
		}
	}
	for _, job := range active {
		desired := quota
		if exceeded && job.Usage.WorkspaceBytes > 0 {
			desired = job.Usage.WorkspaceBytes - 1
		}
		targets := []domain.AllocationMember{{NodeID: job.NodeID, AssignmentID: job.AssignmentID}}
		if job.Allocation != nil {
			targets = job.Allocation.Members
		}
		for _, target := range targets {
			coordinator.storageMu.Lock()
			previous, known := coordinator.storagePolicies[target.AssignmentID]
			if known && previous == desired {
				coordinator.storageMu.Unlock()
				continue
			}
			coordinator.storagePolicies[target.AssignmentID] = desired
			coordinator.storageMu.Unlock()
			command := &xirangv1.ControlMessage{
				MessageId: fmt.Sprintf("command_storage_%s_%d", target.AssignmentID, desired), SentAt: timestamppb.Now(),
				Payload: &xirangv1.ControlMessage_SetAssignmentStoragePolicy{SetAssignmentStoragePolicy: &xirangv1.SetAssignmentStoragePolicy{
					AssignmentId: target.AssignmentID, QuotaBytes: desired,
				}},
			}
			if err := coordinator.commands.Enqueue(target.NodeID, command); err != nil {
				coordinator.storageMu.Lock()
				delete(coordinator.storagePolicies, target.AssignmentID)
				coordinator.storageMu.Unlock()
				return err
			}
		}
	}
	return nil
}

func saturatingUint64Add(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func controllerStorageReserve(total uint64) uint64 {
	reserve := total / 50
	if reserve < 256<<20 {
		reserve = 256 << 20
	}
	if reserve > 4<<30 {
		reserve = 4 << 30
	}
	return reserve
}

func artifactForNode(task domain.TaskSpec, nodeID string) (domain.NativeArtifact, bool) {
	if len(task.ArtifactVariants) == 0 {
		return task.Artifact, true
	}
	for _, variant := range task.ArtifactVariants {
		for _, candidate := range variant.NodeIDs {
			if candidate == nodeID {
				return variant.Artifact, true
			}
		}
	}
	return domain.NativeArtifact{}, false
}

func assignmentCommand(job domain.Job, nodeID string) *xirangv1.ControlMessage {
	artifact, _ := artifactForNode(job.Task, nodeID)
	run := &xirangv1.RunAssignment{
		AssignmentId: job.AssignmentID, JobId: job.ID, Attempt: job.Attempt,
		Environment: cloneStrings(job.Task.Environment), Arguments: append([]string(nil), job.Task.Arguments...),
		WorkingDirectory: job.Task.WorkingDirectory,
		OwnerId:          job.OwnerID, OwnerGroupId: job.GroupID,
		StorageQuotaBytes: uint64(maxInt64(job.StorageQuotaBytes, 0)),
		Resources: &xirangv1.ResourceRequest{
			MilliCpu: job.Task.Resources.MilliCPU, MemoryBytes: uint64(job.Task.Resources.MemoryBytes), Devices: cloneInt64s(job.Task.Resources.Devices),
			CpuSet: job.Task.Placement.CPUSet, NumaNodes: job.Task.Placement.NUMANodes,
		},
		Artifact: &xirangv1.NativeArtifact{
			Uri: artifact.URI, Sha256: artifact.SHA256,
			Architecture: artifact.Architecture, Abi: artifact.ABI,
			Entrypoint: artifact.Entrypoint, Signature: append([]byte(nil), artifact.Signature...),
			RuntimeRequirements: cloneStrings(artifact.RuntimeRequirements),
		},
	}
	if job.Task.Timeout > 0 {
		run.Timeout = durationpb.New(job.Task.Timeout)
	}
	return &xirangv1.ControlMessage{
		MessageId: "command_run_" + job.AssignmentID, SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RunAssignment{RunAssignment: run},
	}
}

func allocationCommand(job domain.Job, member domain.AllocationMember) *xirangv1.ControlMessage {
	message := assignmentCommand(job, member.NodeID)
	run := message.GetRunAssignment()
	run.AssignmentId = member.AssignmentID
	run.AllocationId = job.Allocation.ID
	run.NodeRank = uint32(member.NodeRank)
	run.RankStart = uint32(member.RankStart)
	run.RankCount = uint32(member.RankCount)
	run.WorldSize = uint32(job.Allocation.WorldSize)
	run.Launcher = job.Allocation.Launcher
	run.StartBarrier = true
	run.NetworkInterface = member.NetworkInterface
	run.PrimaryAddress = member.Address
	run.Hosts = make([]*xirangv1.AllocationHost, 0, len(job.Allocation.Hosts))
	for _, host := range job.Allocation.Hosts {
		run.Hosts = append(run.Hosts, &xirangv1.AllocationHost{
			NodeId: host.NodeID, Hostname: host.Hostname, Address: host.Address,
			NetworkInterface: host.NetworkInterface, Slots: uint32(host.Slots),
		})
	}
	message.MessageId = "command_prepare_" + member.AssignmentID
	return message
}

func allocationReleaseCommand(job domain.Job, member domain.AllocationMember) *xirangv1.ControlMessage {
	return &xirangv1.ControlMessage{
		MessageId: "command_release_" + member.AssignmentID, SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_ReleaseAssignment{ReleaseAssignment: &xirangv1.ReleaseAssignment{
			AssignmentId: member.AssignmentID, AllocationId: job.Allocation.ID, JobId: job.ID, Attempt: job.Attempt,
		}},
	}
}

func allocationCancellationCommand(job domain.Job, member domain.AllocationMember) *xirangv1.ControlMessage {
	cancel := &xirangv1.CancelAssignment{
		AssignmentId: member.AssignmentID, Reason: boundedReason(job.Reason), GracePeriod: durationpb.New(job.CancelGracePeriod),
	}
	return &xirangv1.ControlMessage{
		MessageId: "command_cancel_" + member.AssignmentID, SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_CancelAssignment{CancelAssignment: cancel},
	}
}

func maxInt64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

func cancellationCommand(job domain.Job) *xirangv1.ControlMessage {
	cancel := &xirangv1.CancelAssignment{
		AssignmentId: job.AssignmentID, Reason: boundedReason(job.Reason), GracePeriod: durationpb.New(job.CancelGracePeriod),
	}
	return &xirangv1.ControlMessage{
		MessageId: "command_cancel_" + job.AssignmentID, SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_CancelAssignment{CancelAssignment: cancel},
	}
}

func (coordinator *Coordinator) enqueueJobCancellations(job domain.Job) error {
	if job.Allocation == nil {
		return coordinator.commands.Enqueue(job.NodeID, cancellationCommand(job))
	}
	queue, ok := coordinator.commands.(batchCommandQueue)
	if !ok {
		return errors.New("canceling a gang allocation requires atomic command queue support")
	}
	commands := make(map[string][]*xirangv1.ControlMessage)
	for _, member := range job.Allocation.Members {
		if member.State.Terminal() {
			continue
		}
		commands[member.NodeID] = append(commands[member.NodeID], allocationCancellationCommand(job, member))
	}
	if len(commands) == 0 {
		return nil
	}
	return queue.EnqueueBatch(commands)
}

func (coordinator *Coordinator) dispatchAllocationTransition(job domain.Job) error {
	if job.Allocation == nil {
		return nil
	}
	if job.Status == domain.JobStatusCanceling || job.Allocation.State == domain.AllocationStateCanceling {
		return coordinator.enqueueJobCancellations(job)
	}
	if job.Allocation.State != domain.AllocationStateReady {
		return nil
	}
	queue, ok := coordinator.commands.(batchCommandQueue)
	if !ok {
		return errors.New("releasing a gang allocation requires atomic command queue support")
	}
	commands := make(map[string][]*xirangv1.ControlMessage, len(job.Allocation.Members))
	for _, member := range job.Allocation.Members {
		commands[member.NodeID] = append(commands[member.NodeID], allocationReleaseCommand(job, member))
	}
	return queue.EnqueueBatch(commands)
}

func selectNode(job domain.Job, nodes []domain.Node) (domain.Node, bool) {
	return selectNodeWithTopology(job, nodes, nil)
}

func selectNodeWithTopology(job domain.Job, nodes []domain.Node, snapshot *domain.TopologySnapshot) (domain.Node, bool) {
	candidates := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if NodeFitsJob(node, job) && topologyFitsJob(node, job, snapshot) {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 {
		return domain.Node{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		leftPreferred := job.Task.Placement.PreferredZone != "" && candidates[i].Labels["topology.xirang.io/zone"] == job.Task.Placement.PreferredZone
		rightPreferred := job.Task.Placement.PreferredZone != "" && candidates[j].Labels["topology.xirang.io/zone"] == job.Task.Placement.PreferredZone
		if leftPreferred != rightPreferred {
			return leftPreferred
		}
		if candidates[i].ComputePriority != candidates[j].ComputePriority {
			return candidates[i].ComputePriority > candidates[j].ComputePriority
		}
		left, right := nodePressure(candidates[i]), nodePressure(candidates[j])
		if left != right {
			return left < right
		}
		leftWeight, rightWeight := taskComputeWeight(candidates[i], job.Task), taskComputeWeight(candidates[j], job.Task)
		if leftWeight != rightWeight {
			return leftWeight > rightWeight
		}
		leftNetwork, rightNetwork := nodeNetworkCapacity(candidates[i].ID, snapshot), nodeNetworkCapacity(candidates[j].ID, snapshot)
		if leftNetwork != rightNetwork {
			return leftNetwork > rightNetwork
		}
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], true
}

type nodeSetScore struct {
	bottleneck   int64
	worstLatency int64
	healthyPaths int
	priority     int64
	pressure     float64
	weight       float64
}

func selectNodeSetWithTopology(job domain.Job, nodes []domain.Node, count int, snapshot *domain.TopologySnapshot) ([]domain.Node, bool) {
	result := selectNodeSetWithTopologyDetailed(job, nodes, count, snapshot, 100_000)
	return result.nodes, len(result.nodes) == count
}

type nodeSetSearchResult struct {
	nodes           []domain.Node
	visited         int
	budgetExhausted bool
}

func selectNodeSetWithTopologyDetailed(job domain.Job, nodes []domain.Node, count int, snapshot *domain.TopologySnapshot, limit int) nodeSetSearchResult {
	if count < 2 {
		return nodeSetSearchResult{}
	}
	candidates := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if NodeFitsJob(node, job) && topologyFitsJob(node, job, snapshot) {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) < count {
		return nodeSetSearchResult{}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	index := buildTopologyPairIndex(snapshot)
	var best []domain.Node
	var bestScore nodeSetScore
	for seed := range candidates {
		selected := make([]domain.Node, 1, count)
		selected[0] = candidates[seed]
		used := map[string]bool{candidates[seed].ID: true}
		for len(selected) < count {
			var chosen domain.Node
			var chosenScore nodeSetScore
			found := false
			for _, candidate := range candidates {
				if used[candidate.ID] {
					continue
				}
				selected = append(selected, candidate)
				score, feasible := scoreNodeSetIndexed(job, selected, index)
				selected = selected[:len(selected)-1]
				if feasible && (!found || betterNodeSetScore(score, chosenScore)) {
					chosen, chosenScore, found = candidate, score, true
				}
			}
			if !found {
				break
			}
			selected, used[chosen.ID] = append(selected, chosen), true
		}
		if len(selected) != count {
			continue
		}
		score, feasible := scoreNodeSetIndexed(job, selected, index)
		if feasible && (best == nil || betterNodeSetScore(score, bestScore)) {
			best, bestScore = append([]domain.Node(nil), selected...), score
		}
	}
	// Greedy expansion supplies an incumbent quickly. Deterministic bounded
	// backtracking then ranks every visited complete set, so the result is exact
	// when the budget is not exhausted and explicitly approximate otherwise.
	search := findBestNodeSet(job, candidates, count, index, limit, best, bestScore)
	if len(search.nodes) == 0 {
		return search
	}
	sort.Slice(search.nodes, func(i, j int) bool { return search.nodes[i].ID < search.nodes[j].ID })
	return search
}

func findBestNodeSet(job domain.Job, candidates []domain.Node, count int, pairs topologyPairIndex, limit int, incumbent []domain.Node, incumbentScore nodeSetScore) nodeSetSearchResult {
	if count < 1 || len(candidates) < count || limit < 1 {
		return nodeSetSearchResult{}
	}
	result := nodeSetSearchResult{nodes: append([]domain.Node(nil), incumbent...)}
	bestScore := incumbentScore
	selected := make([]domain.Node, 0, count)
	var search func(int)
	search = func(start int) {
		if len(selected) == count {
			score, feasible := scoreNodeSetIndexed(job, selected, pairs)
			if feasible && (len(result.nodes) == 0 || betterNodeSetScore(score, bestScore)) {
				result.nodes, bestScore = append([]domain.Node(nil), selected...), score
			}
			return
		}
		if len(selected)+len(candidates)-start < count {
			return
		}
		for index := start; index < len(candidates); index++ {
			if result.visited >= limit {
				result.budgetExhausted = true
				return
			}
			result.visited++
			selected = append(selected, candidates[index])
			_, feasible := scoreNodeSetIndexed(job, selected, pairs)
			if feasible {
				search(index + 1)
			}
			selected = selected[:len(selected)-1]
			if result.budgetExhausted {
				return
			}
		}
	}
	search(0)
	return result
}

func scoreNodeSetIndexed(job domain.Job, nodes []domain.Node, index topologyPairIndex) (nodeSetScore, bool) {
	score := nodeSetScore{bottleneck: math.MaxInt64}
	selectedPaths := make(map[string]string, len(nodes))
	for _, node := range nodes {
		score.priority += int64(node.ComputePriority)
		score.pressure += nodePressure(node)
		score.weight += taskComputeWeight(node, job.Task)
	}
	for left := 0; left < len(nodes); left++ {
		for right := left + 1; right < len(nodes); right++ {
			pair, found := index[topologyPairKey(nodes[left].ID, nodes[right].ID)]
			if !found {
				if job.Task.Placement.MinimumNetworkBits > 0 {
					return nodeSetScore{}, false
				}
				pair.capacity, pair.latency = 0, math.MaxInt64
			}
			if pair.capacity < job.Task.Placement.MinimumNetworkBits {
				return nodeSetScore{}, false
			}
			if pair.capacity < score.bottleneck {
				score.bottleneck = pair.capacity
			}
			if pair.latency > score.worstLatency {
				score.worstLatency = pair.latency
			}
			if pair.health == "healthy" {
				score.healthyPaths++
			}
			if job.Task.Placement.MinimumNetworkBits > 0 && pair.enforcePath {
				for nodeID, signature := range map[string]string{
					nodes[left].ID: pair.pathFor(nodes[left].ID), nodes[right].ID: pair.pathFor(nodes[right].ID),
				} {
					if signature == "" || selectedPaths[nodeID] != "" && selectedPaths[nodeID] != signature {
						return nodeSetScore{}, false
					}
					selectedPaths[nodeID] = signature
				}
			}
		}
	}
	if score.bottleneck == math.MaxInt64 {
		score.bottleneck = 0
	}
	return score, true
}

type topologyPairValue struct {
	capacity        int64
	latency         int64
	health          string
	edgeID          string
	sourceID        string
	sourceInterface string
	sourceAddress   string
	targetID        string
	targetInterface string
	targetAddress   string
	enforcePath     bool
}

func (pair topologyPairValue) pathFor(nodeID string) string {
	if nodeID == pair.sourceID {
		return pair.sourceInterface + "\x00" + pair.sourceAddress
	}
	if nodeID == pair.targetID {
		return pair.targetInterface + "\x00" + pair.targetAddress
	}
	return ""
}

type topologyPairIndex map[string]topologyPairValue

func topologyPairKey(left, right string) string {
	if left > right {
		left, right = right, left
	}
	return left + "\x00" + right
}

func buildTopologyPairIndex(snapshot *domain.TopologySnapshot) topologyPairIndex {
	index := make(topologyPairIndex)
	if snapshot == nil {
		return index
	}
	edges := make(map[string]domain.TopologyEdge, len(snapshot.Edges))
	for _, edge := range snapshot.Edges {
		edges[edge.ID] = edge
	}
	selected := make(map[string]bool, len(snapshot.PathSelections))
	for _, selection := range snapshot.PathSelections {
		key := topologyPairKey(selection.SourceID, selection.TargetID)
		selected[key] = true
		edge, found := edges[selection.PrimaryEdgeID]
		if !found {
			continue
		}
		candidate := topologyPairValue{capacity: selection.EffectiveCapacityBits, latency: topologyLatency(selection.EffectiveLatencyMicros),
			health: selection.Health, edgeID: selection.PrimaryEdgeID, sourceID: selection.SourceID, targetID: selection.TargetID,
			enforcePath: edge.Type == "probe"}
		candidate.sourceInterface, candidate.sourceAddress = endpointPath(edge, selection.SourceID)
		candidate.targetInterface, candidate.targetAddress = endpointPath(edge, selection.TargetID)
		if candidate.targetInterface == "" && selection.PrimaryReverseEdgeID != "" {
			candidate.targetInterface, candidate.targetAddress = endpointPath(edges[selection.PrimaryReverseEdgeID], selection.TargetID)
		}
		index[key] = candidate
	}
	for _, edge := range snapshot.Edges {
		if edge.Health == "critical" {
			continue
		}
		key := topologyPairKey(edge.SourceID, edge.TargetID)
		if selected[key] {
			continue
		}
		candidate := topologyPairValue{capacity: edge.CapacityBits, latency: topologyLatency(edge.LatencyMicros), health: edge.Health, edgeID: edge.ID,
			sourceID: edge.SourceID, sourceInterface: edge.SourceInterface, sourceAddress: edge.SourceAddress,
			targetID: edge.TargetID, targetInterface: edge.TargetInterface, targetAddress: edge.TargetAddress}
		current, found := index[key]
		if !found || topologyHealthRank(edge.Health) > topologyHealthRank(current.health) ||
			(topologyHealthRank(edge.Health) == topologyHealthRank(current.health) && (candidate.capacity > current.capacity ||
				(candidate.capacity == current.capacity && candidate.latency < current.latency))) {
			index[key] = candidate
		}
	}
	return index
}

func endpointPath(edge domain.TopologyEdge, nodeID string) (string, string) {
	if edge.SourceID == nodeID {
		return edge.SourceInterface, edge.SourceAddress
	}
	if edge.TargetID == nodeID {
		return edge.TargetInterface, edge.TargetAddress
	}
	return "", ""
}

// A non-positive probe latency means that the value is unavailable. Keep that
// sentinel out of ranking arithmetic: zero would otherwise outrank every
// measured path even though it is not a zero-latency observation.
func topologyLatency(latency int64) int64 {
	if latency <= 0 {
		return math.MaxInt64
	}
	return latency
}

func betterNodeSetScore(left, right nodeSetScore) bool {
	if left.bottleneck != right.bottleneck {
		return left.bottleneck > right.bottleneck
	}
	if left.worstLatency != right.worstLatency {
		return left.worstLatency < right.worstLatency
	}
	if left.healthyPaths != right.healthyPaths {
		return left.healthyPaths > right.healthyPaths
	}
	if left.priority != right.priority {
		return left.priority > right.priority
	}
	if left.pressure != right.pressure {
		return left.pressure < right.pressure
	}
	if left.weight != right.weight {
		return left.weight > right.weight
	}
	return false
}

func (coordinator *Coordinator) newAllocation(job domain.Job, nodes []domain.Node, snapshot *domain.TopologySnapshot) (domain.Allocation, error) {
	parallel := job.Task.Parallel.Effective()
	if len(nodes) != int(parallel.Nodes) {
		return domain.Allocation{}, ErrNoCapacity
	}
	allocationID, err := randomID(coordinator.random, "allocation_")
	if err != nil {
		return domain.Allocation{}, err
	}
	allocation := domain.Allocation{
		ID: allocationID, State: domain.AllocationStatePreparing, WorldSize: parallel.WorldSize(), Launcher: parallel.Launcher,
		Members: make([]domain.AllocationMember, 0, len(nodes)), Hosts: make([]domain.AllocationHost, 0, len(nodes)),
		TaskGroups: []domain.TaskGroup{{ID: "main", Launcher: parallel.Launcher, Ranks: make([]domain.RankAssignment, 0, parallel.WorldSize())}},
	}
	for index, node := range nodes {
		assignmentID, err := randomID(coordinator.random, "assignment_")
		if err != nil {
			return domain.Allocation{}, err
		}
		interfaceName, address := preferredNodePath(node.ID, nodes, snapshot)
		if address == "" {
			address = strings.TrimSpace(node.ManagementAddress)
		}
		if address == "" {
			address = node.Name
		}
		rankStart := int32(index) * parallel.TasksPerNode
		allocation.Members = append(allocation.Members, domain.AllocationMember{
			NodeID: node.ID, AssignmentID: assignmentID, NodeRank: int32(index), RankStart: rankStart,
			RankCount: parallel.TasksPerNode, NetworkInterface: interfaceName, Address: address,
		})
		allocation.Hosts = append(allocation.Hosts, domain.AllocationHost{
			NodeID: node.ID, Hostname: node.Name, Address: address, NetworkInterface: interfaceName, Slots: parallel.TasksPerNode,
		})
		for localRank := int32(0); localRank < parallel.TasksPerNode; localRank++ {
			allocation.TaskGroups[0].Ranks = append(allocation.TaskGroups[0].Ranks, domain.RankAssignment{
				Rank: rankStart + localRank, LocalRank: localRank, NodeRank: int32(index), NodeID: node.ID,
			})
		}
	}
	return allocation, nil
}

func preferredNodePath(nodeID string, nodes []domain.Node, snapshot *domain.TopologySnapshot) (string, string) {
	selected := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		selected[node.ID] = true
	}
	if snapshot != nil {
		index := buildTopologyPairIndex(snapshot)
		peers := make([]string, 0, len(selected))
		for peerID := range selected {
			if peerID != nodeID {
				peers = append(peers, peerID)
			}
		}
		sort.Strings(peers)
		for _, peerID := range peers {
			pair, ok := index[topologyPairKey(nodeID, peerID)]
			if !ok {
				continue
			}
			parts := strings.SplitN(pair.pathFor(nodeID), "\x00", 2)
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				return parts[0], parts[1]
			}
		}
	}
	return "", ""
}

// NodeFitsJob is exported for transactional repositories to repeat the decision
// against current state and prevent time-of-check/time-of-use overcommit.
func NodeFitsJob(node domain.Node, job domain.Job) bool {
	if !nodeMatchesJob(node, job) {
		return false
	}
	available, err := node.Allocatable.Subtract(node.Allocated)
	return err == nil && available.Fits(job.Task.Resources)
}

func nodeMatchesJob(node domain.Node, job domain.Job) bool {
	if !node.Schedulable() {
		return false
	}
	if !containsText(node.Capabilities, "cgroup-v2-executor-v1") {
		return false
	}
	if job.Task.Placement.RequiredNodeID != "" && node.ID != job.Task.Placement.RequiredNodeID {
		return false
	}
	parallel := job.Task.Parallel.Effective()
	if parallel.Nodes > 1 && !containsText(node.Capabilities, "gang-assignment-v1") {
		return false
	}
	if parallel.Nodes > 1 && parallel.Launcher != "native" && !containsText(node.Capabilities, "mpi-launch-v1/"+parallel.Launcher) {
		return false
	}
	artifact, artifactAvailable := artifactForNode(job.Task, node.ID)
	if !artifactAvailable {
		return false
	}
	requiredArchitecture := job.Task.Placement.RequiredArchitecture
	if requiredArchitecture == "" {
		requiredArchitecture = artifact.Architecture
	}
	if requiredArchitecture != "" && node.Architecture != requiredArchitecture {
		return false
	}
	if !runtimecompat.Satisfies(artifact.RuntimeRequirements, node.RuntimeFacts) {
		return false
	}
	for key, value := range job.Task.Placement.RequiredLabels {
		if node.Labels[key] != value {
			return false
		}
	}
	for key, value := range job.Task.Placement.RequiredTopology {
		if node.Labels[key] != value {
			return false
		}
	}
	for _, excluded := range job.Task.Placement.ExcludedNodeIDs {
		if node.ID == excluded {
			return false
		}
	}
	for _, taint := range node.Taints {
		if (taint.Effect == "NoSchedule" || taint.Effect == "NoExecute") && !tolerates(job.Task.Placement.Tolerations, taint) {
			return false
		}
	}
	if len(job.Task.Resources.Devices) > 0 && !containsText(node.Capabilities, "device-resource-executor-v1") {
		return false
	}
	for resource := range job.Task.Resources.Devices {
		if !containsText(node.Capabilities, "device-resource-executor-v1/"+resource) {
			return false
		}
	}
	if (job.Task.Placement.CPUSet != "" || job.Task.Placement.NUMANodes != "") && !containsText(node.Capabilities, "cpuset-affinity-v1") {
		return false
	}
	if !nodeSupportsAffinity(node, job.Task.Placement.CPUSet, job.Task.Placement.NUMANodes, job.Task.Resources.Devices) {
		return false
	}
	return true
}

func selectPreemptionPlan(
	preemptor domain.Job,
	jobs []domain.Job,
	nodes []domain.Node,
	topology *domain.TopologySnapshot,
	policies map[string]domain.QueuePolicy,
) (domain.Node, []domain.Job, bool) {
	type plan struct {
		node    domain.Node
		victims []domain.Job
	}
	plans := make([]plan, 0)
	for _, node := range nodes {
		if !nodeMatchesJob(node, preemptor) || !topologyFitsJob(node, preemptor, topology) {
			continue
		}
		available, err := node.Allocatable.Subtract(node.Allocated)
		if err != nil {
			continue
		}
		candidates := make([]domain.Job, 0)
		for _, candidate := range jobs {
			if candidate.NodeID != node.ID || candidate.Priority >= preemptor.Priority ||
				(candidate.Status != domain.JobStatusAssigned && candidate.Status != domain.JobStatusRunning) ||
				candidate.PreemptedByJobID != "" {
				continue
			}
			victimPolicy := effectiveQueuePolicy(candidate.Queue, policies)
			maxPreemptions := victimPolicy.MaxPreemptionsPerJob
			if maxPreemptions == 0 {
				maxPreemptions = 3
			}
			if !victimPolicy.Preemptible || candidate.PreemptionCount >= uint32(maxPreemptions) {
				continue
			}
			candidates = append(candidates, candidate)
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Priority != candidates[j].Priority {
				return candidates[i].Priority < candidates[j].Priority
			}
			leftStarted, rightStarted := candidates[i].StartedAt, candidates[j].StartedAt
			if leftStarted != nil && rightStarted != nil && !leftStarted.Equal(*rightStarted) {
				return leftStarted.After(*rightStarted)
			}
			return candidates[i].ID < candidates[j].ID
		})
		victims := make([]domain.Job, 0)
		for _, candidate := range candidates {
			available = available.Add(candidate.Task.Resources)
			victims = append(victims, candidate)
			if available.Fits(preemptor.Task.Resources) {
				plans = append(plans, plan{node: node, victims: victims})
				break
			}
		}
	}
	if len(plans) == 0 {
		return domain.Node{}, nil, false
	}
	sort.Slice(plans, func(i, j int) bool {
		if len(plans[i].victims) != len(plans[j].victims) {
			return len(plans[i].victims) < len(plans[j].victims)
		}
		var leftPriority, rightPriority int64
		for _, victim := range plans[i].victims {
			leftPriority += int64(victim.Priority)
		}
		for _, victim := range plans[j].victims {
			rightPriority += int64(victim.Priority)
		}
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return plans[i].node.ID < plans[j].node.ID
	})
	return plans[0].node, plans[0].victims, true
}

func victimIDs(jobs []domain.Job) []string {
	ids := make([]string, len(jobs))
	for index := range jobs {
		ids[index] = jobs[index].ID
	}
	return ids
}

func resourceVectorEmpty(vector domain.ResourceVector) bool {
	if vector.MilliCPU != 0 || vector.MemoryBytes != 0 || vector.EphemeralStorageBytes != 0 {
		return false
	}
	for _, quantity := range vector.Devices {
		if quantity != 0 {
			return false
		}
	}
	return true
}

func componentMax(left, right domain.ResourceVector) domain.ResourceVector {
	result := left
	if right.MilliCPU > result.MilliCPU {
		result.MilliCPU = right.MilliCPU
	}
	if right.MemoryBytes > result.MemoryBytes {
		result.MemoryBytes = right.MemoryBytes
	}
	if right.EphemeralStorageBytes > result.EphemeralStorageBytes {
		result.EphemeralStorageBytes = right.EphemeralStorageBytes
	}
	if result.Devices == nil {
		result.Devices = map[string]int64{}
	}
	for name, quantity := range right.Devices {
		if quantity > result.Devices[name] {
			result.Devices[name] = quantity
		}
	}
	return result
}

func positiveDeficit(demand, supply domain.ResourceVector) domain.ResourceVector {
	result := domain.ResourceVector{Devices: map[string]int64{}}
	if demand.MilliCPU > supply.MilliCPU {
		result.MilliCPU = demand.MilliCPU - supply.MilliCPU
	}
	if demand.MemoryBytes > supply.MemoryBytes {
		result.MemoryBytes = demand.MemoryBytes - supply.MemoryBytes
	}
	if demand.EphemeralStorageBytes > supply.EphemeralStorageBytes {
		result.EphemeralStorageBytes = demand.EphemeralStorageBytes - supply.EphemeralStorageBytes
	}
	for name, quantity := range demand.Devices {
		if quantity > supply.Devices[name] {
			result.Devices[name] = quantity - supply.Devices[name]
		}
	}
	return result
}

func nodesForDeficit(deficit, representative domain.ResourceVector) int32 {
	required := int64(0)
	ceil := func(value, unit int64) int64 {
		if value <= 0 {
			return 0
		}
		if unit <= 0 {
			return 1<<31 - 1
		}
		return (value + unit - 1) / unit
	}
	for _, value := range []int64{
		ceil(deficit.MilliCPU, representative.MilliCPU),
		ceil(deficit.MemoryBytes, representative.MemoryBytes),
		ceil(deficit.EphemeralStorageBytes, representative.EphemeralStorageBytes),
	} {
		if value > required {
			required = value
		}
	}
	for name, quantity := range deficit.Devices {
		if value := ceil(quantity, representative.Devices[name]); value > required {
			required = value
		}
	}
	if required > 1<<31-1 {
		return 1<<31 - 1
	}
	return int32(required)
}

func nodesForBudget(budget, representative domain.ResourceVector) int32 {
	limits := make([]int64, 0)
	if representative.MilliCPU > 0 && budget.MilliCPU > 0 {
		limits = append(limits, budget.MilliCPU/representative.MilliCPU)
	}
	if representative.MemoryBytes > 0 && budget.MemoryBytes > 0 {
		limits = append(limits, budget.MemoryBytes/representative.MemoryBytes)
	}
	for name, quantity := range representative.Devices {
		if quantity > 0 && budget.Devices[name] > 0 {
			limits = append(limits, budget.Devices[name]/quantity)
		}
	}
	if len(limits) == 0 {
		return 0
	}
	result := limits[0]
	for _, limit := range limits[1:] {
		if limit < result {
			result = limit
		}
	}
	if result > 1<<31-1 {
		return 1<<31 - 1
	}
	return int32(result)
}

func tolerates(tolerations []domain.Toleration, taint domain.Taint) bool {
	for _, toleration := range tolerations {
		if toleration.Key == taint.Key && toleration.Effect == taint.Effect && (toleration.Value == "" || toleration.Value == taint.Value) {
			return true
		}
	}
	return false
}

func topologyFitsJob(node domain.Node, job domain.Job, snapshot *domain.TopologySnapshot) bool {
	minimum := job.Task.Placement.MinimumNetworkBits
	return minimum <= 0 || nodeNetworkCapacity(node.ID, snapshot) >= minimum
}

func nodeNetworkCapacity(nodeID string, snapshot *domain.TopologySnapshot) int64 {
	if snapshot == nil {
		return 0
	}
	var capacity int64
	for _, selection := range snapshot.PathSelections {
		if selection.SourceID == nodeID || selection.TargetID == nodeID {
			if selection.EffectiveCapacityBits > capacity {
				capacity = selection.EffectiveCapacityBits
			}
		}
	}
	if len(snapshot.PathSelections) > 0 {
		return capacity
	}
	type candidate struct {
		health   int
		capacity int64
	}
	bestByPeer := make(map[string]candidate)
	for _, edge := range snapshot.Edges {
		if edge.Health == "critical" || (edge.SourceID != nodeID && edge.TargetID != nodeID) {
			continue
		}
		peer := edge.TargetID
		if peer == nodeID {
			peer = edge.SourceID
		}
		health := topologyHealthRank(edge.Health)
		current := bestByPeer[peer]
		if health > current.health || (health == current.health && edge.CapacityBits > current.capacity) {
			bestByPeer[peer] = candidate{health: health, capacity: edge.CapacityBits}
		}
	}
	for _, current := range bestByPeer {
		if current.capacity > capacity {
			capacity = current.capacity
		}
	}
	return capacity
}

func topologyHealthRank(health string) int {
	switch health {
	case "healthy":
		return 3
	case "degraded":
		return 2
	default:
		return 1
	}
}

func containsText(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func nodePressure(node domain.Node) float64 {
	cpu, memory := float64(0), float64(0)
	if node.Allocatable.MilliCPU > 0 {
		cpu = float64(node.Allocated.MilliCPU) / float64(node.Allocatable.MilliCPU)
	}
	if node.Allocatable.MemoryBytes > 0 {
		memory = float64(node.Allocated.MemoryBytes) / float64(node.Allocatable.MemoryBytes)
	}
	if memory > cpu {
		return memory
	}
	return cpu
}

func validateSubmission(request SubmitRequest) error {
	if !validText(strings.TrimSpace(request.ProjectID), 128, false) || !validText(strings.TrimSpace(request.Queue), 128, true) || request.Priority < -1000 || request.Priority > 1000 ||
		request.MaxAttempts < 0 || request.MaxAttempts > 100 || request.StorageQuotaBytes < 0 ||
		request.ArrayIndex < 0 || request.ArraySize < 0 || request.ArraySize > 1000 ||
		(request.ArraySize > 0 && !validText(request.ArrayID, 256, false)) ||
		len(request.Dependencies) > 64 {
		return ErrInvalidJob
	}
	for _, dependency := range request.Dependencies {
		if !validText(dependency.JobID, 256, false) || (dependency.Condition != "afterok" && dependency.Condition != "afterany") {
			return ErrInvalidJob
		}
	}
	if err := request.Task.Resources.Validate(); err != nil || request.Task.Resources.MilliCPU < 10 || request.Task.Resources.MemoryBytes <= 0 {
		return fmt.Errorf("%w: scheduling requires positive CPU and memory", ErrInvalidJob)
	}
	parallel := request.Task.Parallel.Effective()
	if parallel.Nodes < 1 || parallel.Nodes > 128 || parallel.TasksPerNode < 1 || parallel.TasksPerNode > 256 ||
		int64(parallel.Nodes)*int64(parallel.TasksPerNode) > 4096 ||
		(parallel.Launcher != "native" && parallel.Launcher != "openmpi" && parallel.Launcher != "mpich" && parallel.Launcher != "intelmpi") {
		return fmt.Errorf("%w: invalid parallel allocation", ErrInvalidJob)
	}
	if request.Task.Resources.MilliCPU > math.MaxInt64/int64(parallel.Nodes) || request.Task.Resources.MemoryBytes > math.MaxInt64/int64(parallel.Nodes) {
		return fmt.Errorf("%w: parallel resource total overflows", ErrInvalidJob)
	}
	for _, quantity := range request.Task.Resources.Devices {
		if quantity > math.MaxInt64/int64(parallel.Nodes) {
			return fmt.Errorf("%w: parallel device total overflows", ErrInvalidJob)
		}
	}
	if request.Task.Resources.EphemeralStorageBytes != 0 {
		return fmt.Errorf("%w: job storage is controlled by administrator quota and cannot be requested", ErrInvalidJob)
	}
	if len(request.Task.Arguments) > 256 || len(request.Task.Environment) > 256 ||
		request.Task.Timeout < 0 || request.Task.User != "" || request.Task.Group != "" {
		return ErrInvalidJob
	}
	if request.Task.WorkingDirectory != "" && !safeRelativePath(request.Task.WorkingDirectory) {
		return ErrInvalidJob
	}
	if len(request.Task.ArtifactVariants) == 0 {
		if request.Task.Application != "" || validateNativeArtifact(request.Task.Artifact) != nil {
			return ErrInvalidJob
		}
		if request.Task.Placement.RequiredArchitecture != "" && request.Task.Placement.RequiredArchitecture != request.Task.Artifact.Architecture {
			return fmt.Errorf("%w: placement and artifact architectures differ", ErrInvalidJob)
		}
	} else {
		if !validText(request.Task.Application, 128, false) || len(request.Task.ArtifactVariants) > 1024 || request.Task.Artifact.URI != "" {
			return ErrInvalidJob
		}
		mapped := make(map[string]struct{})
		matchingArchitecture := request.Task.Placement.RequiredArchitecture == ""
		for _, variant := range request.Task.ArtifactVariants {
			if !validText(variant.BuildID, 256, false) || !validText(variant.Variant, 128, false) || len(variant.NodeIDs) == 0 || len(variant.NodeIDs) > 1024 || validateNativeArtifact(variant.Artifact) != nil {
				return ErrInvalidJob
			}
			if request.Task.Placement.RequiredArchitecture == variant.Artifact.Architecture {
				matchingArchitecture = true
			}
			for _, nodeID := range variant.NodeIDs {
				if !validText(nodeID, 256, false) {
					return ErrInvalidJob
				}
				// Resolver order defines the winner for overlaps; keep the first.
				if _, exists := mapped[nodeID]; !exists {
					mapped[nodeID] = struct{}{}
				}
			}
		}
		if !matchingArchitecture {
			return fmt.Errorf("%w: no application variant matches the required architecture", ErrInvalidJob)
		}
	}
	if len(request.Task.Placement.RequiredLabels) > 64 || len(request.Task.Placement.RequiredTopology) > 64 ||
		!validText(request.Task.Placement.PreferredZone, 128, true) ||
		!validText(request.Task.Placement.RequiredNodeID, 256, true) ||
		(request.Task.Placement.CPUSet != "" && !validLinuxIDList(request.Task.Placement.CPUSet)) ||
		(request.Task.Placement.NUMANodes != "" && !validLinuxIDList(request.Task.Placement.NUMANodes)) {
		return ErrInvalidJob
	}
	for key, value := range request.Task.Placement.RequiredLabels {
		if !validText(key, 128, false) || !validText(value, 256, false) {
			return ErrInvalidJob
		}
	}
	for key, value := range request.Task.Placement.RequiredTopology {
		if !validText(key, 128, false) || !validText(value, 256, false) {
			return ErrInvalidJob
		}
	}
	if len(request.Task.Placement.ExcludedNodeIDs) > 256 || len(request.Task.Placement.Tolerations) > 64 || request.Task.Placement.MinimumNetworkBits < 0 {
		return ErrInvalidJob
	}
	for _, nodeID := range request.Task.Placement.ExcludedNodeIDs {
		if !validText(nodeID, 256, false) {
			return ErrInvalidJob
		}
	}
	for _, toleration := range request.Task.Placement.Tolerations {
		if !validText(toleration.Key, 128, false) || !validText(toleration.Value, 256, true) || (toleration.Effect != "NoSchedule" && toleration.Effect != "NoExecute") {
			return ErrInvalidJob
		}
	}
	if len(request.Task.Resources.Devices) > 64 {
		return ErrInvalidJob
	}
	for name, quantity := range request.Task.Resources.Devices {
		if !validText(name, 128, false) || quantity <= 0 || quantity > 1_000_000 {
			return ErrInvalidJob
		}
	}
	for _, argument := range request.Task.Arguments {
		if !validText(argument, 4096, true) {
			return ErrInvalidJob
		}
	}
	for name, value := range request.Task.Environment {
		if !validText(name, 256, false) || strings.ContainsAny(name, "=") || !validText(value, 8192, true) {
			return ErrInvalidJob
		}
	}
	return nil
}

func validateNativeArtifact(artifact domain.NativeArtifact) error {
	if !validText(artifact.URI, 4096, false) || !validText(artifact.Architecture, 64, false) || !validText(artifact.Entrypoint, 512, false) ||
		len(artifact.SHA256) != sha256.Size*2 || !safeRelativePath(artifact.Entrypoint) {
		return ErrInvalidJob
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil {
		return ErrInvalidJob
	}
	parsed, err := url.Parse(artifact.URI)
	if err != nil || (parsed.Scheme != "file" && parsed.Scheme != "https") {
		return fmt.Errorf("%w: artifact URI must use file or HTTPS", ErrInvalidJob)
	}
	return nil
}

func nodeSupportsAffinity(node domain.Node, cpuSet, numaNodes string, devices map[string]int64) bool {
	requiredCPUs, ok := expandLinuxIDList(cpuSet)
	if !ok {
		return false
	}
	requiredNUMA, ok := expandLinuxIDList(numaNodes)
	if !ok {
		return false
	}
	availableCPUs := make(map[uint32]bool)
	availableNUMA := make(map[uint32]bool)
	for _, numa := range node.NUMATopology {
		availableNUMA[numa.ID] = true
		cpus, valid := expandLinuxIDList(numa.CPUs)
		if !valid {
			continue
		}
		for cpu := range cpus {
			availableCPUs[cpu] = true
		}
	}
	for cpu := range requiredCPUs {
		if !availableCPUs[cpu] {
			return false
		}
	}
	for numa := range requiredNUMA {
		if !availableNUMA[numa] {
			return false
		}
	}
	if len(requiredNUMA) > 0 && len(devices) > 0 {
		for resource, quantity := range devices {
			var local int64
			for numa := range requiredNUMA {
				local += node.DeviceTopology[resource][strconv.FormatUint(uint64(numa), 10)]
			}
			if local < quantity {
				return false
			}
		}
	}
	return true
}

func validLinuxIDList(value string) bool {
	_, ok := expandLinuxIDList(value)
	return ok
}

func expandLinuxIDList(value string) (map[uint32]bool, bool) {
	result := make(map[uint32]bool)
	if value == "" {
		return result, true
	}
	if len(value) > 4096 {
		return nil, false
	}
	for _, part := range strings.Split(value, ",") {
		startText, endText, found := strings.Cut(part, "-")
		start, err := strconv.ParseUint(startText, 10, 32)
		if err != nil {
			return nil, false
		}
		end := start
		if found {
			end, err = strconv.ParseUint(endText, 10, 32)
			if err != nil || end < start || end-start > 1_000_000 {
				return nil, false
			}
		}
		for current := start; current <= end; current++ {
			result[uint32(current)] = true
		}
	}
	return result, true
}

func safeRelativePath(value string) bool {
	return value != "" && !strings.ContainsRune(value, '\x00') && !path.IsAbs(value) && path.Clean(value) == value && value != "." && !strings.HasPrefix(value, "../")
}

func validText(value string, maximum int, emptyAllowed bool) bool {
	return (emptyAllowed || value != "") && len(value) <= maximum && !strings.ContainsRune(value, '\x00')
}

func randomID(reader func([]byte) (int, error), prefix string) (string, error) {
	buffer := make([]byte, 16)
	read, err := reader(buffer)
	if err != nil {
		return "", err
	}
	if read != len(buffer) {
		return "", errors.New("short cryptographic random read")
	}
	return prefix + hex.EncodeToString(buffer), nil
}

func cloneTask(task domain.TaskSpec) domain.TaskSpec {
	cloned := task
	cloned.Arguments = append([]string(nil), task.Arguments...)
	cloned.Environment = cloneStrings(task.Environment)
	cloned.Placement.RequiredLabels = cloneStrings(task.Placement.RequiredLabels)
	cloned.Placement.RequiredTopology = cloneStrings(task.Placement.RequiredTopology)
	cloned.Placement.ExcludedNodeIDs = append([]string(nil), task.Placement.ExcludedNodeIDs...)
	cloned.Placement.Tolerations = append([]domain.Toleration(nil), task.Placement.Tolerations...)
	cloned.Resources.Devices = cloneInt64s(task.Resources.Devices)
	cloned.Artifact.Signature = append([]byte(nil), task.Artifact.Signature...)
	cloned.ArtifactVariants = make([]domain.NativeArtifactVariant, len(task.ArtifactVariants))
	for index, variant := range task.ArtifactVariants {
		cloned.ArtifactVariants[index] = variant
		cloned.ArtifactVariants[index].NodeIDs = append([]string(nil), variant.NodeIDs...)
		cloned.ArtifactVariants[index].Artifact.Signature = append([]byte(nil), variant.Artifact.Signature...)
	}
	return cloned
}

func cloneInt64s(values map[string]int64) map[string]int64 {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]int64, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneStrings(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func boundedReason(reason string) string {
	if len(reason) > 4096 {
		return reason[:4096]
	}
	return reason
}

func protocolAssignmentState(state xirangv1.AssignmentState) (domain.AssignmentState, bool) {
	switch state {
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_ACCEPTED:
		return domain.AssignmentStateAccepted, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_PULLING:
		return domain.AssignmentStatePulling, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_READY:
		return domain.AssignmentStateReady, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING:
		return domain.AssignmentStateRunning, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_SUSPENDED:
		return domain.AssignmentStateSuspended, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED:
		return domain.AssignmentStateSucceeded, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED:
		return domain.AssignmentStateFailed, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED:
		return domain.AssignmentStateCanceled, true
	default:
		return "", false
	}
}
