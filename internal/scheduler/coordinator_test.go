package scheduler_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/controlstream"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/pki"
	"github.com/xirang-grid/xirang/internal/scheduler"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type commandQueue struct {
	nodeID  string
	message *xirangv1.ControlMessage
	batches []map[string][]*xirangv1.ControlMessage
}

func (queue *commandQueue) Enqueue(nodeID string, message *xirangv1.ControlMessage) error {
	queue.nodeID, queue.message = nodeID, message
	return nil
}

func (queue *commandQueue) EnqueueBatch(commands map[string][]*xirangv1.ControlMessage) error {
	cloned := make(map[string][]*xirangv1.ControlMessage, len(commands))
	for nodeID, messages := range commands {
		cloned[nodeID] = append([]*xirangv1.ControlMessage(nil), messages...)
		for _, message := range messages {
			queue.nodeID, queue.message = nodeID, message
		}
	}
	queue.batches = append(queue.batches, cloned)
	return nil
}

type topologySource struct{ snapshot domain.TopologySnapshot }

func (source topologySource) LatestSnapshot(context.Context) (domain.TopologySnapshot, error) {
	return source.snapshot, nil
}

func TestCoordinatorSchedulesArchitectureAndReleasesReservation(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentService := enrollment.NewService(store, authority)
	token, err := enrollmentService.CreateToken(ctx, enrollment.TokenOptions{Name: "scheduler nodes", TTL: time.Hour, MaxUses: 2, RequireApproval: false})
	if err != nil {
		t.Fatal(err)
	}
	amd64 := enrollNode(t, enrollmentService, token.Secret, "amd64-node", "amd64")
	arm64 := enrollNode(t, enrollmentService, token.Secret, "arm64-node", "arm64")
	queue := &commandQueue{}
	coordinator := scheduler.New(store, store, queue)
	job, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "project-a", Queue: "batch", Priority: 50, MaxAttempts: 2,
		Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///srv/xirang/task", SHA256: strings.Repeat("a", 64), Architecture: "arm64", Entrypoint: "task"},
			Resources: domain.ResourceVector{MilliCPU: 2_000, MemoryBytes: 2 << 30},
			Placement: domain.Placement{RequiredArchitecture: "arm64"}, Timeout: time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	assigned, err := coordinator.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assigned.Status != domain.JobStatusAssigned || assigned.NodeID != arm64.ID || queue.nodeID != arm64.ID || queue.message.GetRunAssignment() == nil {
		t.Fatalf("unexpected assignment: job=%#v queue=%#v", assigned, queue)
	}
	if queue.message.GetRunAssignment().Resources.GetMilliCpu() != 2_000 || queue.message.GetRunAssignment().Resources.GetMemoryBytes() != 2<<30 {
		t.Fatalf("assignment did not carry enforceable resource limits: %#v", queue.message.GetRunAssignment().Resources)
	}
	reservedARM, err := store.GetNode(ctx, arm64.ID)
	if err != nil {
		t.Fatal(err)
	}
	reservedAMD, err := store.GetNode(ctx, amd64.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reservedARM.Allocated.MilliCPU != 2_000 || reservedARM.Allocated.MemoryBytes != 2<<30 || reservedAMD.Allocated.MilliCPU != 0 {
		t.Fatalf("unexpected reservations arm=%#v amd=%#v", reservedARM.Allocated, reservedAMD.Allocated)
	}
	blocked, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "project-b", Queue: "batch", Priority: 10,
		Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///srv/xirang/large", SHA256: strings.Repeat("c", 64), Architecture: "arm64", Entrypoint: "large"},
			Resources: domain.ResourceVector{MilliCPU: 7_000, MemoryBytes: 1 << 30},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	stillQueued, err := coordinator.Get(ctx, blocked.ID)
	if err != nil || stillQueued.Status != domain.JobStatusQueued {
		t.Fatalf("capacity-constrained job should remain queued: job=%#v err=%v", stillQueued, err)
	}
	if stillQueued.BlockReason == nil || stillQueued.BlockReason.Code != "resource_capacity" || stillQueued.BlockReason.Category != "resource" || stillQueued.BlockReason.Details["requiredArchitecture"] != "arm64" {
		t.Fatalf("queued job lacks structured resource diagnostics: %#v", stillQueued.BlockReason)
	}
	backfill, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "project-backfill", Queue: "batch", Priority: -10,
		Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///srv/xirang/small", SHA256: strings.Repeat("d", 64), Architecture: "arm64", Entrypoint: "small"},
			Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	backfilled, err := coordinator.Get(ctx, backfill.ID)
	if err != nil || backfilled.Status != domain.JobStatusAssigned {
		t.Fatalf("smaller lower-priority job did not backfill idle capacity: job=%#v err=%v", backfilled, err)
	}
	if err := coordinator.ReportAssignment(ctx, arm64.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: backfilled.AssignmentID, Attempt: backfilled.Attempt,
		State: xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, OccurredAt: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.UpdateNode(ctx, arm64.ID, func(node *domain.Node) error {
		node.Status = domain.NodeStatusUnreachable
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	orphaned, err := coordinator.Get(ctx, job.ID)
	if err != nil || orphaned.Status != domain.JobStatusOrphaned {
		t.Fatalf("unreachable assignment was not reconciled as orphaned: job=%#v err=%v", orphaned, err)
	}
	reservedOrphan, err := store.GetNode(ctx, arm64.ID)
	if err != nil || reservedOrphan.Allocated.MilliCPU != 2_000 {
		t.Fatalf("orphan reconciliation released resources: node=%#v err=%v", reservedOrphan, err)
	}
	if _, err := store.UpdateNode(ctx, arm64.ID, func(node *domain.Node) error {
		node.Status = domain.NodeStatusReady
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ReportAssignment(ctx, arm64.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: assigned.AssignmentID, Attempt: assigned.Attempt,
		State: xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, OccurredAt: timestamppb.New(now.Add(-time.Second)),
	}); err != nil {
		t.Fatal(err)
	}
	reconciled, err := coordinator.Get(ctx, job.ID)
	if err != nil || reconciled.Status != domain.JobStatusRunning {
		t.Fatalf("Agent snapshot did not reconcile orphaned job: job=%#v err=%v", reconciled, err)
	}
	if err := coordinator.ReportAssignmentLog(ctx, arm64.ID, &xirangv1.AssignmentLogSnapshot{
		AssignmentId: assigned.AssignmentID, Attempt: assigned.Attempt, StdoutTail: []byte("live\n"),
		ObservedAt: timestamppb.New(now),
	}); err != nil {
		t.Fatal(err)
	}
	withLiveLogs, err := coordinator.Get(ctx, job.ID)
	if err != nil || withLiveLogs.StdoutTail != "live\n" {
		t.Fatalf("live log snapshot was not persisted: job=%#v err=%v", withLiveLogs, err)
	}
	for index, state := range []xirangv1.AssignmentState{
		xirangv1.AssignmentState_ASSIGNMENT_STATE_ACCEPTED,
		xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING,
		xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED,
	} {
		update := &xirangv1.AssignmentUpdate{
			AssignmentId: assigned.AssignmentID, Attempt: assigned.Attempt, State: state,
			OccurredAt: timestamppb.New(now.Add(time.Duration(index) * time.Second)),
		}
		if state == xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED {
			update.StdoutTail, update.StderrTail, update.LogsTruncated = []byte("result\n"), []byte("warning\n"), true
		}
		if err := coordinator.ReportAssignment(ctx, arm64.ID, update); err != nil {
			t.Fatal(err)
		}
	}
	completed, err := coordinator.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	released, err := store.GetNode(ctx, arm64.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != domain.JobStatusSucceeded || completed.StartedAt == nil || completed.FinishedAt == nil || completed.StdoutTail != "result\n" || completed.StderrTail != "warning\n" || !completed.LogsTruncated || released.Allocated.MilliCPU != 0 || released.Allocated.MemoryBytes != 0 {
		t.Fatalf("completion did not release resources: job=%#v node=%#v", completed, released.Allocated)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	unblocked, err := coordinator.Get(ctx, blocked.ID)
	if err != nil || unblocked.Status != domain.JobStatusAssigned || unblocked.NodeID != arm64.ID {
		t.Fatalf("released capacity did not unblock job: job=%#v err=%v", unblocked, err)
	}
	restoredQueue := &commandQueue{}
	if err := scheduler.New(store, store, restoredQueue).RestoreAssignments(ctx); err != nil {
		t.Fatal(err)
	}
	if restoredQueue.message == nil || restoredQueue.message.GetRunAssignment().AssignmentId != unblocked.AssignmentID || restoredQueue.message.GetRunAssignment().Attempt != unblocked.Attempt {
		t.Fatalf("restart did not restore immutable assignment: %#v", restoredQueue.message)
	}
	canceling, err := coordinator.Cancel(ctx, blocked.ID, scheduler.CancelRequest{Reason: "operator request", GracePeriod: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if canceling.Status != domain.JobStatusCanceling || queue.message.GetCancelAssignment() == nil || queue.message.GetCancelAssignment().AssignmentId != unblocked.AssignmentID || queue.message.GetCancelAssignment().GracePeriod.AsDuration() != 2*time.Second {
		t.Fatalf("running job cancellation was not dispatched: job=%#v command=%#v", canceling, queue.message)
	}
	if err := coordinator.ReportAssignment(ctx, arm64.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: unblocked.AssignmentID, Attempt: unblocked.Attempt,
		State: xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, OccurredAt: timestamppb.Now(), Reason: "assignment canceled",
	}); err != nil {
		t.Fatal(err)
	}
	canceled, err := coordinator.Get(ctx, blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	releasedAgain, err := store.GetNode(ctx, arm64.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != domain.JobStatusCanceled || releasedAgain.Allocated.MilliCPU != 0 || releasedAgain.Allocated.MemoryBytes != 0 {
		t.Fatalf("cancellation did not release reservation: job=%#v resources=%#v", canceled, releasedAgain.Allocated)
	}
	rejectedJob, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "project-c",
		Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///srv/xirang/rejected", SHA256: strings.Repeat("e", 64), Architecture: "arm64", Entrypoint: "rejected"},
			Resources: domain.ResourceVector{MilliCPU: 1000, MemoryBytes: 1 << 30},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	assignedRejected, err := coordinator.Get(ctx, rejectedJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Cancel(ctx, rejectedJob.ID, scheduler.CancelRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ConfirmAssignmentCommand(ctx, arm64.ID, &xirangv1.RunAssignment{
		AssignmentId: assignedRejected.AssignmentID, JobId: assignedRejected.ID, Attempt: assignedRejected.Attempt,
	}, false, "rejected before execution"); err != nil {
		t.Fatal(err)
	}
	canceledBeforeStart, err := coordinator.Get(ctx, rejectedJob.ID)
	if err != nil || canceledBeforeStart.Status != domain.JobStatusCanceled {
		t.Fatalf("run rejection during cancellation was not terminal: job=%#v err=%v", canceledBeforeStart, err)
	}
}

func TestCoordinatorPreservesPriorityOrder(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	coordinator := scheduler.New(store, store, &commandQueue{})
	request := scheduler.SubmitRequest{
		ProjectID: "project-a", Queue: "default",
		Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///tmp/task", SHA256: strings.Repeat("b", 64), Architecture: "amd64", Entrypoint: "task"},
			Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30},
		},
	}
	request.Priority = 1
	low, err := coordinator.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Priority = 100
	high, err := coordinator.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := coordinator.List(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != high.ID || jobs[1].ID != low.ID {
		t.Fatalf("priority order was not preserved: %#v", jobs)
	}
}

func TestCoordinatorRejectsImpossiblePerNodeResourcesButAllowsTemporaryPressure(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentService := enrollment.NewService(store, authority)
	token, err := enrollmentService.CreateToken(ctx, enrollment.TokenOptions{Name: "capacity", TTL: time.Hour, MaxUses: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enrollmentService.Register(ctx, token.Secret, testCSR(t), domain.InventorySummary{
		Hostname: "eight-core", OS: "linux", Architecture: "amd64", CPUCount: 8, MemoryBytes: 16 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := scheduler.New(store, store, &commandQueue{})
	request := scheduler.SubmitRequest{ProjectID: "research", Task: domain.TaskSpec{
		Artifact:  domain.NativeArtifact{URI: "file:///task", SHA256: strings.Repeat("a", 64), Architecture: "amd64", Entrypoint: "task"},
		Resources: domain.ResourceVector{MilliCPU: 9_000, MemoryBytes: 1 << 30},
	}}
	_, err = coordinator.Submit(ctx, request)
	var impossible *scheduler.UnsatisfiableError
	if !errors.As(err, &impossible) || impossible.Reason.Code != "per_node_capacity_impossible" || impossible.Reason.Details["largestNodeMilliCPU"] != "8000" {
		t.Fatalf("expected structured impossible-capacity error, got %#v (%v)", impossible, err)
	}
	jobs, listErr := coordinator.List(ctx, 10)
	if listErr != nil || len(jobs) != 0 {
		t.Fatalf("impossible job was persisted: jobs=%#v err=%v", jobs, listErr)
	}
	request.Task.Resources.MilliCPU = 8_000
	if _, err := coordinator.Submit(ctx, request); err != nil {
		t.Fatalf("job that fits total node capacity must remain admissible: %v", err)
	}
}

func TestCoordinatorRejectsGangWhenTooFewNodesCanEverFit(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentService := enrollment.NewService(store, authority)
	token, err := enrollmentService.CreateToken(ctx, enrollment.TokenOptions{Name: "gang capacity", TTL: time.Hour, MaxUses: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, hostname := range []string{"large", "small"} {
		memory := int64(16 << 30)
		if hostname == "small" {
			memory = 2 << 30
		}
		if _, err := enrollmentService.Register(ctx, token.Secret, testCSR(t), domain.InventorySummary{
			Hostname: hostname, OS: "linux", Architecture: "amd64", CPUCount: 8, MemoryBytes: memory,
		}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := scheduler.New(store, store, &commandQueue{})
	_, err = coordinator.Submit(ctx, scheduler.SubmitRequest{ProjectID: "mpi", Task: domain.TaskSpec{
		Artifact:  domain.NativeArtifact{URI: "file:///task", SHA256: strings.Repeat("a", 64), Architecture: "amd64", Entrypoint: "task"},
		Resources: domain.ResourceVector{MilliCPU: 4_000, MemoryBytes: 8 << 30},
		Parallel:  domain.ParallelSpec{Nodes: 2, TasksPerNode: 1, Launcher: "native"},
	}})
	var impossible *scheduler.UnsatisfiableError
	if !errors.As(err, &impossible) || impossible.Reason.Code != "allocation_node_count_impossible" || impossible.Reason.Details["capableNodes"] != "1" {
		t.Fatalf("expected permanent gang-capacity rejection, got %#v (%v)", impossible, err)
	}
}

func TestCoordinatorGangAllocationUsesTopologyBarrierAndAtomicRelease(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentService := enrollment.NewService(store, authority)
	token, err := enrollmentService.CreateToken(ctx, enrollment.TokenOptions{Name: "gang nodes", TTL: time.Hour, MaxUses: 3})
	if err != nil {
		t.Fatal(err)
	}
	first := enrollNode(t, enrollmentService, token.Secret, "gang-a", "amd64")
	second := enrollNode(t, enrollmentService, token.Secret, "gang-b", "amd64")
	third := enrollNode(t, enrollmentService, token.Secret, "gang-c", "amd64")
	snapshot := domain.TopologySnapshot{Edges: []domain.TopologyEdge{
		{ID: "fast-a-b", SourceID: first.ID, TargetID: second.ID, SourceInterface: "fabric0", SourceAddress: "10.25.0.1", CapacityBits: 25_000_000_000, LatencyMicros: 20, Health: "healthy"},
		{ID: "fast-b-a", SourceID: second.ID, TargetID: first.ID, SourceInterface: "fabric0", SourceAddress: "10.25.0.2", CapacityBits: 25_000_000_000, LatencyMicros: 20, Health: "healthy"},
		{ID: "slow-a-c", SourceID: first.ID, TargetID: third.ID, SourceInterface: "eth0", SourceAddress: "10.10.0.1", CapacityBits: 10_000_000_000, LatencyMicros: 80, Health: "healthy"},
		{ID: "slow-b-c", SourceID: second.ID, TargetID: third.ID, SourceInterface: "eth0", SourceAddress: "10.10.0.2", CapacityBits: 10_000_000_000, LatencyMicros: 90, Health: "healthy"},
	}}
	queue := &commandQueue{}
	coordinator := scheduler.New(store, store, queue)
	coordinator.SetTopologySource(topologySource{snapshot: snapshot})
	job, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		OwnerID: "scientist", ProjectID: "mpi", Queue: "batch", MaxAttempts: 1,
		Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///srv/xirang/spmd", SHA256: strings.Repeat("a", 64), Architecture: "amd64", Entrypoint: "spmd"},
			Resources: domain.ResourceVector{MilliCPU: 2_000, MemoryBytes: 2 << 30},
			Parallel:  domain.ParallelSpec{Nodes: 2, TasksPerNode: 2, Launcher: "native"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	assigned, err := coordinator.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assigned.Allocation == nil || assigned.Allocation.WorldSize != 4 || len(assigned.Allocation.Members) != 2 || len(queue.batches) != 1 {
		t.Fatalf("gang allocation was not atomically prepared: job=%#v batches=%#v", assigned, queue.batches)
	}
	restoredPrepare := &commandQueue{}
	if err := scheduler.New(store, store, restoredPrepare).RestoreAssignments(ctx); err != nil {
		t.Fatal(err)
	}
	if len(restoredPrepare.batches) != 1 || len(restoredPrepare.batches[0]) != 2 {
		t.Fatalf("controller restart did not restore all gang prepare commands: %#v", restoredPrepare.batches)
	}
	for _, messages := range restoredPrepare.batches[0] {
		if len(messages) != 1 || messages[0].GetRunAssignment() == nil {
			t.Fatalf("restart restored a non-prepare command: %#v", messages)
		}
	}
	selected := map[string]bool{}
	for _, member := range assigned.Allocation.Members {
		selected[member.NodeID] = true
		messages := queue.batches[0][member.NodeID]
		if len(messages) != 1 || !messages[0].GetRunAssignment().GetStartBarrier() || messages[0].GetRunAssignment().GetWorldSize() != 4 {
			t.Fatalf("member prepare command is incomplete: %#v", messages)
		}
	}
	if !selected[first.ID] || !selected[second.ID] || selected[third.ID] {
		t.Fatalf("topology scheduler did not choose the 25G pair: %#v", selected)
	}
	for _, node := range []domain.Node{first, second, third} {
		stored, err := store.GetNode(ctx, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if selected[node.ID] {
			want = 2_000
		}
		if stored.Allocated.MilliCPU != want {
			t.Fatalf("node %s allocated CPU = %d, want %d", node.ID, stored.Allocated.MilliCPU, want)
		}
	}
	for index, member := range assigned.Allocation.Members {
		if err := coordinator.ReportAssignment(ctx, member.NodeID, &xirangv1.AssignmentUpdate{
			AssignmentId: member.AssignmentID, Attempt: assigned.Attempt,
			State: xirangv1.AssignmentState_ASSIGNMENT_STATE_READY, OccurredAt: timestamppb.New(time.Now().Add(time.Duration(index) * time.Millisecond)),
		}); err != nil {
			t.Fatal(err)
		}
		if index == 0 && len(queue.batches) != 1 {
			t.Fatal("release barrier opened before every member was ready")
		}
	}
	if len(queue.batches) != 2 {
		t.Fatalf("all-ready transition did not emit one atomic release batch: %#v", queue.batches)
	}
	restoredRelease := &commandQueue{}
	if err := scheduler.New(store, store, restoredRelease).RestoreAssignments(ctx); err != nil {
		t.Fatal(err)
	}
	if len(restoredRelease.batches) != 1 || len(restoredRelease.batches[0]) != 2 {
		t.Fatalf("controller restart did not restore the gang release barrier: %#v", restoredRelease.batches)
	}
	for _, messages := range restoredRelease.batches[0] {
		if len(messages) != 1 || messages[0].GetReleaseAssignment() == nil {
			t.Fatalf("restart restored a non-release command: %#v", messages)
		}
	}
	for _, member := range assigned.Allocation.Members {
		messages := queue.batches[1][member.NodeID]
		if len(messages) != 1 || messages[0].GetReleaseAssignment().GetAllocationId() != assigned.Allocation.ID {
			t.Fatalf("invalid release command for %s: %#v", member.NodeID, messages)
		}
		if err := coordinator.ReportAssignment(ctx, member.NodeID, &xirangv1.AssignmentUpdate{
			AssignmentId: member.AssignmentID, Attempt: assigned.Attempt,
			State: xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, OccurredAt: timestamppb.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, member := range assigned.Allocation.Members {
		factor := uint64(index + 1)
		if err := coordinator.ReportAssignmentUsage(ctx, member.NodeID, &xirangv1.AssignmentUsageSnapshot{
			AssignmentId: member.AssignmentID, Attempt: assigned.Attempt,
			CpuUsageMicros: 100 * factor, MemoryByteSeconds: 1_000 * factor,
			GpuDeviceSeconds: factor, WallTimeMillis: 10 * factor, WorkspaceBytes: 20 * factor,
			FilesystemAvailableBytes: 100 << 30, FilesystemTotalBytes: 200 << 30, ObservedAt: timestamppb.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	accounted, err := coordinator.Get(ctx, job.ID)
	if err != nil || accounted.Usage.CPUUsageMicros != 300 || accounted.Usage.MemoryByteSeconds != 3_000 ||
		accounted.Usage.GPUDeviceSeconds != 3 || accounted.Usage.WorkspaceBytes != 60 {
		t.Fatalf("gang usage was not aggregated across members: usage=%#v err=%v", accounted.Usage, err)
	}
	for _, member := range assigned.Allocation.Members {
		if err := coordinator.ReportAssignment(ctx, member.NodeID, &xirangv1.AssignmentUpdate{
			AssignmentId: member.AssignmentID, Attempt: assigned.Attempt,
			State: xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, OccurredAt: timestamppb.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	completed, err := coordinator.Get(ctx, job.ID)
	if err != nil || completed.Status != domain.JobStatusSucceeded || completed.Allocation.State != domain.AllocationStateSucceeded {
		t.Fatalf("gang did not complete as one job: job=%#v err=%v", completed, err)
	}
	for nodeID := range selected {
		stored, err := store.GetNode(ctx, nodeID)
		if err != nil || stored.Allocated.MilliCPU != 0 {
			t.Fatalf("gang resources were not released on %s: node=%#v err=%v", nodeID, stored, err)
		}
	}
}

func TestCoordinatorGangCommandFailureRollsBackEveryReservation(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := enrollment.NewService(store, authority)
	token, err := service.CreateToken(ctx, enrollment.TokenOptions{Name: "rollback nodes", TTL: time.Hour, MaxUses: 2})
	if err != nil {
		t.Fatal(err)
	}
	first := enrollNode(t, service, token.Secret, "rollback-a", "amd64")
	second := enrollNode(t, service, token.Secret, "rollback-b", "amd64")
	hub := controlstream.NewHub(1)
	if err := hub.Enqueue(first.ID, &xirangv1.ControlMessage{
		MessageId: "existing", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_SetNodeMode{SetNodeMode: &xirangv1.SetNodeMode{Mode: xirangv1.NodeMode_NODE_MODE_READY}},
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := scheduler.New(store, store, hub)
	job, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "rollback", Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///tmp/spmd", SHA256: strings.Repeat("a", 64), Architecture: "amd64", Entrypoint: "spmd"},
			Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30},
			Parallel:  domain.ParallelSpec{Nodes: 2, TasksPerNode: 1, Launcher: "native"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); !errors.Is(err, controlstream.ErrCommandQueueFull) {
		t.Fatalf("schedule failure = %v", err)
	}
	rolledBack, err := coordinator.Get(ctx, job.ID)
	if err != nil || rolledBack.Status != domain.JobStatusQueued || rolledBack.Allocation != nil || rolledBack.Attempt != 0 {
		t.Fatalf("job reservation was not fully rolled back: job=%#v err=%v", rolledBack, err)
	}
	for _, node := range []domain.Node{first, second} {
		stored, err := store.GetNode(ctx, node.ID)
		if err != nil || stored.Allocated.MilliCPU != 0 || stored.Allocated.MemoryBytes != 0 {
			t.Fatalf("node reservation leaked on %s: node=%#v err=%v", node.ID, stored, err)
		}
	}
}

func TestCoordinatorGangMemberFailureCancelsPeersAndRestoresCancelingPhase(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := enrollment.NewService(store, authority)
	token, err := service.CreateToken(ctx, enrollment.TokenOptions{Name: "failure nodes", TTL: time.Hour, MaxUses: 2})
	if err != nil {
		t.Fatal(err)
	}
	enrollNode(t, service, token.Secret, "failure-a", "amd64")
	enrollNode(t, service, token.Secret, "failure-b", "amd64")
	queue := &commandQueue{}
	coordinator := scheduler.New(store, store, queue)
	job, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "failure", MaxAttempts: 1, Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///tmp/spmd", SHA256: strings.Repeat("b", 64), Architecture: "amd64", Entrypoint: "spmd"},
			Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30},
			Parallel:  domain.ParallelSpec{Nodes: 2, TasksPerNode: 1, Launcher: "native"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	assigned, err := coordinator.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed, peer := assigned.Allocation.Members[0], assigned.Allocation.Members[1]
	if err := coordinator.ReportAssignment(ctx, failed.NodeID, &xirangv1.AssignmentUpdate{
		AssignmentId: failed.AssignmentID, Attempt: assigned.Attempt,
		State: xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, ExitCode: 17,
		Reason: "injected prepare failure", OccurredAt: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	canceling, err := coordinator.Get(ctx, job.ID)
	if err != nil || canceling.Status != domain.JobStatusCanceling || canceling.Allocation.State != domain.AllocationStateCanceling {
		t.Fatalf("member failure did not atomically move the gang to canceling: job=%#v err=%v", canceling, err)
	}
	if len(queue.batches) != 2 || len(queue.batches[1]) != 1 || queue.batches[1][peer.NodeID][0].GetCancelAssignment() == nil {
		t.Fatalf("member failure did not cancel the surviving peer: %#v", queue.batches)
	}
	restoredCancel := &commandQueue{}
	if err := scheduler.New(store, store, restoredCancel).RestoreAssignments(ctx); err != nil {
		t.Fatal(err)
	}
	if len(restoredCancel.batches) != 1 || len(restoredCancel.batches[0]) != 1 || restoredCancel.batches[0][peer.NodeID][0].GetCancelAssignment() == nil {
		t.Fatalf("controller restart did not restore gang cancellation: %#v", restoredCancel.batches)
	}
	if err := coordinator.ReportAssignment(ctx, peer.NodeID, &xirangv1.AssignmentUpdate{
		AssignmentId: peer.AssignmentID, Attempt: assigned.Attempt,
		State: xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, Reason: "peer failed", OccurredAt: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	terminal, err := coordinator.Get(ctx, job.ID)
	if err != nil || terminal.Status != domain.JobStatusFailed || terminal.Allocation.State != domain.AllocationStateFailed || terminal.ExitCode != 17 {
		t.Fatalf("gang did not finish as one failed job after all members terminated: job=%#v err=%v", terminal, err)
	}
	for _, member := range assigned.Allocation.Members {
		node, err := store.GetNode(ctx, member.NodeID)
		if err != nil || node.Allocated.MilliCPU != 0 || node.Allocated.MemoryBytes != 0 {
			t.Fatalf("terminal gang leaked resources on %s: node=%#v err=%v", member.NodeID, node, err)
		}
	}
}

func TestCoordinatorCancelsQueuedJobWithoutAgentCommand(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queue := &commandQueue{}
	coordinator := scheduler.New(store, store, queue)
	job, err := coordinator.Submit(context.Background(), scheduler.SubmitRequest{
		ProjectID: "project-c",
		Task: domain.TaskSpec{
			Artifact:  domain.NativeArtifact{URI: "file:///tmp/task", SHA256: strings.Repeat("d", 64), Architecture: "amd64", Entrypoint: "task"},
			Resources: domain.ResourceVector{MilliCPU: 1000, MemoryBytes: 1 << 30},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := coordinator.Cancel(context.Background(), job.ID, scheduler.CancelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != domain.JobStatusCanceled || canceled.FinishedAt == nil || queue.message != nil {
		t.Fatalf("queued cancellation was not immediate: job=%#v command=%#v", canceled, queue.message)
	}
}

func TestCoordinatorPreemptionWaitsForAgentAndRequeuesVictim(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentService := enrollment.NewService(store, authority)
	token, err := enrollmentService.CreateToken(ctx, enrollment.TokenOptions{Name: "preemption node", TTL: time.Hour, MaxUses: 1})
	if err != nil {
		t.Fatal(err)
	}
	node := enrollNode(t, enrollmentService, token.Secret, "preemption-node", "amd64")
	queue := &commandQueue{}
	coordinator := scheduler.New(store, store, queue)
	if _, err := coordinator.PutQueuePolicy(ctx, domain.QueuePolicy{
		Name: "batch", Weight: 1, Backfill: true, Preemptible: true, MaxPreemptionsPerJob: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.PutQueuePolicy(ctx, domain.QueuePolicy{
		Name: "urgent", Weight: 10, Backfill: true, AllowPreemption: true, PreemptionGraceSeconds: 7,
		MaxRecommendedScaleOut: 2,
		ElasticityBudget:       domain.ResourceVector{MilliCPU: 16_000, MemoryBytes: 32 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	task := func(name string, milliCPU int64) domain.TaskSpec {
		return domain.TaskSpec{
			Artifact: domain.NativeArtifact{
				URI: "file:///tmp/" + name, SHA256: strings.Repeat("a", 64),
				Architecture: "amd64", Entrypoint: name,
			},
			Resources: domain.ResourceVector{MilliCPU: milliCPU, MemoryBytes: 2 << 30},
		}
	}
	victim, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "science", Queue: "batch", Priority: 10, MaxAttempts: 1, Task: task("batch", 6_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	runningVictim, err := coordinator.Get(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ReportAssignment(ctx, node.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: runningVictim.AssignmentID, Attempt: runningVictim.Attempt,
		State: xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, OccurredAt: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	preemptor, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "operations", Queue: "urgent", Priority: 100, Task: task("urgent", 7_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	canceling, err := coordinator.Get(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := coordinator.Get(ctx, preemptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceling.Status != domain.JobStatusCanceling || canceling.PreemptedByJobID != preemptor.ID ||
		canceling.PreemptionCount != 1 || queue.message.GetCancelAssignment() == nil ||
		queue.message.GetCancelAssignment().GracePeriod.AsDuration() != 7*time.Second {
		t.Fatalf("preemption was not durably dispatched: victim=%#v command=%#v", canceling, queue.message)
	}
	if waiting.Status != domain.JobStatusQueued || len(waiting.PreemptionVictims) != 1 ||
		waiting.PreemptionVictims[0] != victim.ID || reserved.Allocated.MilliCPU != 6_000 {
		t.Fatalf("preemptor ran before confirmed release: preemptor=%#v allocated=%#v", waiting, reserved.Allocated)
	}
	replayed := &commandQueue{}
	if err := scheduler.New(store, store, replayed).RestoreAssignments(ctx); err != nil {
		t.Fatal(err)
	}
	if replayed.message.GetCancelAssignment() == nil || replayed.message.GetCancelAssignment().AssignmentId != canceling.AssignmentID {
		t.Fatalf("restart did not replay the durable cancellation: %#v", replayed.message)
	}
	if err := coordinator.ConfirmAssignmentCancellation(ctx, node.ID, replayed.message.GetCancelAssignment(), false, "temporary Agent refusal"); err != nil {
		t.Fatal(err)
	}
	restoredVictim, err := coordinator.Get(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	clearedPlan, err := coordinator.Get(ctx, preemptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	stillReserved, err := store.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restoredVictim.Status != domain.JobStatusRunning || restoredVictim.PreemptedByJobID != "" ||
		len(clearedPlan.PreemptionVictims) != 0 || stillReserved.Allocated.MilliCPU != 6_000 {
		t.Fatalf("rejected cancellation did not roll back safely: victim=%#v preemptor=%#v allocated=%#v", restoredVictim, clearedPlan, stillReserved.Allocated)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	canceling, err = coordinator.Get(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceling.Status != domain.JobStatusCanceling || canceling.PreemptionCount != 2 {
		t.Fatalf("eligible preemption was not retried after rejection: %#v", canceling)
	}
	elasticity, err := coordinator.Elasticity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(elasticity.Recommendations) != 1 || elasticity.Recommendations[0].Queue != "urgent" ||
		elasticity.Recommendations[0].SuggestedAdditionalNodes != 1 {
		t.Fatalf("unexpected bounded elasticity advice: %#v", elasticity)
	}
	if err := coordinator.ReportAssignment(ctx, node.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: canceling.AssignmentID, Attempt: canceling.Attempt,
		State: xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, Reason: "terminated for preemption",
		OccurredAt: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := coordinator.Get(ctx, victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	unblocked, err := coordinator.Get(ctx, preemptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	released, err := store.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != domain.JobStatusQueued || requeued.AssignmentID != "" ||
		unblocked.Status != domain.JobStatusQueued || len(unblocked.PreemptionVictims) != 0 ||
		released.Allocated.MilliCPU != 0 {
		t.Fatalf("confirmed preemption did not safely release and requeue: victim=%#v preemptor=%#v node=%#v", requeued, unblocked, released.Allocated)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	assignedPreemptor, err := coordinator.Get(ctx, preemptor.ID)
	if err != nil || assignedPreemptor.Status != domain.JobStatusAssigned {
		t.Fatalf("preemptor was not assigned after release: job=%#v err=%v", assignedPreemptor, err)
	}
	events, err := store.ListAuditEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Action != "job.preemption.confirmed" {
		t.Fatalf("preemption audit trail is incomplete: %#v", events)
	}
}

func enrollNode(t *testing.T, service *enrollment.Service, token, name, architecture string) domain.Node {
	t.Helper()
	registered, err := service.Register(context.Background(), token, testCSR(t), domain.InventorySummary{
		Hostname: name, OS: "linux", Architecture: architecture, CPUCount: 8, MemoryBytes: 16 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := []string{"native-executor-v1", "cgroup-v2-executor-v1", "gang-assignment-v1"}
	if err := service.ReportCapabilities(context.Background(), registered.Node.ID, capabilities); err != nil {
		t.Fatal(err)
	}
	registered.Node.Capabilities = capabilities
	return registered.Node
}

func testCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// TestSchedulingResolvesDependenciesFinishedOutsideTheFairShareWindow guards the
// hazard created by bounding what Schedule reads. dependencyState fails a job
// outright when a prerequisite is absent from the map it is given, so a
// prerequisite that completed long ago must still be resolved rather than
// treated as having disappeared.
func TestSchedulingResolvesDependenciesFinishedOutsideTheFairShareWindow(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentService := enrollment.NewService(store, authority)
	token, err := enrollmentService.CreateToken(ctx, enrollment.TokenOptions{Name: "dependency nodes", TTL: time.Hour, MaxUses: 1, RequireApproval: false})
	if err != nil {
		t.Fatal(err)
	}
	enrollNode(t, enrollmentService, token.Secret, "amd64-node", "amd64")
	queue := &commandQueue{}
	coordinator := scheduler.New(store, store, queue)

	task := domain.TaskSpec{
		Artifact:  domain.NativeArtifact{URI: "file:///srv/xirang/task", SHA256: strings.Repeat("a", 64), Architecture: "amd64", Entrypoint: "task"},
		Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30},
		Timeout:   time.Minute,
	}
	prerequisite, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "project-a", Queue: "batch", MaxAttempts: 1, Task: task,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Age the prerequisite far past any fair-share horizon, so it can only be
	// found by the dependency lookup and not by the scheduling window.
	longAgo := time.Now().UTC().Add(-5 * 365 * 24 * time.Hour)
	if _, err := store.FailQueuedJob(ctx, prerequisite.ID, "completed long ago", longAgo); err != nil {
		t.Fatal(err)
	}

	dependent, err := coordinator.Submit(ctx, scheduler.SubmitRequest{
		ProjectID: "project-a", Queue: "batch", MaxAttempts: 1, Task: task,
		Dependencies: []domain.JobDependency{{JobID: prerequisite.ID, Condition: "afterany"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	scheduled, err := coordinator.Get(ctx, dependent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if scheduled.Status == domain.JobStatusFailed {
		t.Fatalf("a job was failed because its long-finished prerequisite was not resolved: %s", scheduled.Reason)
	}
	if scheduled.Status != domain.JobStatusAssigned {
		t.Fatalf("the dependent job did not schedule after its prerequisite completed: status=%s reason=%s",
			scheduled.Status, scheduled.Reason)
	}
}
