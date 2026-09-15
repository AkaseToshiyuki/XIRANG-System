package scheduler

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type batchNodeSource []domain.Node

func (source batchNodeSource) ListNodes(context.Context) ([]domain.Node, error) { return source, nil }

func TestFairJobLessPrefersLowerWeightedShareThenProject(t *testing.T) {
	stats := &schedulingStats{
		queueCount:   map[string]int32{"fast": 4, "batch": 1},
		projectCount: map[string]int32{"fast\x00a": 4, "batch\x00b": 1},
		queueUsage:   map[string]domain.ResourceVector{}, projectUsage: map[string]domain.ResourceVector{},
	}
	policies := map[string]domain.QueuePolicy{
		"fast":  {Name: "fast", Weight: 4, Backfill: true},
		"batch": {Name: "batch", Weight: 1, Backfill: true},
	}
	older := time.Now().Add(-time.Minute)
	fast := domain.Job{ID: "fast", Queue: "fast", ProjectID: "a", Priority: 1, CreatedAt: older}
	batch := domain.Job{ID: "batch", Queue: "batch", ProjectID: "b", Priority: 100, CreatedAt: older}
	if !fairJobLess(batch, fast, stats, policies) {
		t.Fatal("equal weighted shares did not honor priority")
	}
	stats.queueCount["fast"] = 8
	if !fairJobLess(batch, fast, stats, policies) {
		t.Fatal("lower weighted queue share was not preferred")
	}
}

func TestPolicyAllowsJobEnforcesConcurrencyAndResourceQuotas(t *testing.T) {
	job := domain.Job{Queue: "batch", ProjectID: "research", Task: domain.TaskSpec{Resources: domain.ResourceVector{MilliCPU: 2_000, MemoryBytes: 2 << 30}, Timeout: 30 * 24 * time.Hour}}
	stats := &schedulingStats{
		queueCount: map[string]int32{"batch": 1}, projectCount: map[string]int32{"batch\x00research": 1},
		queueUsage:   map[string]domain.ResourceVector{"batch": {MilliCPU: 2_000, MemoryBytes: 2 << 30}},
		projectUsage: map[string]domain.ResourceVector{"batch\x00research": {MilliCPU: 2_000, MemoryBytes: 2 << 30}},
	}
	policy := domain.QueuePolicy{Name: "batch", Weight: 1, Backfill: true, MaxConcurrent: 2, MaxConcurrentPerProject: 2,
		ResourceQuota:        domain.ResourceVector{MilliCPU: 4_000, MemoryBytes: 4 << 30},
		ProjectResourceQuota: domain.ResourceVector{MilliCPU: 4_000, MemoryBytes: 4 << 30},
	}
	if !policyAllowsJob(policy, job, stats) {
		t.Fatal("job at exact policy limits was rejected")
	}
	policy.MaxWallTimeSeconds = 1
	if !policyAllowsJob(policy, job, stats) {
		t.Fatal("deprecated queue wall-time field overrode the user-group runtime policy")
	}
	policy.MaxConcurrentPerProject = 1
	if policyAllowsJob(policy, job, stats) {
		t.Fatal("per-project concurrency limit was ignored")
	}
	policy.MaxConcurrentPerProject = 2
	policy.ResourceQuota.MilliCPU = 3_999
	if policyAllowsJob(policy, job, stats) {
		t.Fatal("queue CPU quota was ignored")
	}
}

func TestNodeFitsJobEnforcesTaintsTopologyAndDeviceCapability(t *testing.T) {
	node := domain.Node{
		ID: "gpu-node", Status: domain.NodeStatusReady, Architecture: "amd64",
		Capabilities: []string{"cgroup-v2-executor-v1", "device-resource-executor-v1", "device-resource-executor-v1/nvidia.com/gpu"},
		Labels:       map[string]string{"topology.xirang.io/zone": "east", "topology.xirang.io/rack": "r1"},
		Taints:       []domain.Taint{{Key: "accelerator", Value: "gpu", Effect: "NoSchedule"}},
		Allocatable:  domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30, Devices: map[string]int64{"nvidia.com/gpu": 2}},
	}
	job := domain.Job{Task: domain.TaskSpec{
		Artifact:  domain.NativeArtifact{Architecture: "amd64"},
		Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30, Devices: map[string]int64{"nvidia.com/gpu": 1}},
		Placement: domain.Placement{RequiredTopology: map[string]string{"topology.xirang.io/rack": "r1"}},
	}}
	if NodeFitsJob(node, job) {
		t.Fatal("untolerated GPU taint was ignored")
	}
	job.Task.Placement.Tolerations = []domain.Toleration{{Key: "accelerator", Value: "gpu", Effect: "NoSchedule"}}
	if !NodeFitsJob(node, job) {
		t.Fatal("valid toleration and named device capacity did not fit")
	}
	job.Task.Placement.ExcludedNodeIDs = []string{node.ID}
	if NodeFitsJob(node, job) {
		t.Fatal("excluded node was selected")
	}
	job.Task.Placement.ExcludedNodeIDs = nil
	node.Capabilities = []string{"cgroup-v2-executor-v1"}
	if NodeFitsJob(node, job) {
		t.Fatal("device request reached a node without device enforcement capability")
	}
}

func TestNodeFitsJobEnforcesRuntimeContract(t *testing.T) {
	node := domain.Node{ID: "runtime-node", Status: domain.NodeStatusReady, Architecture: "amd64",
		Capabilities: []string{"cgroup-v2-executor-v1"}, Allocatable: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30},
		RuntimeFacts: map[string]string{"os": "linux", "arch": "amd64", "libc.version": "2.39"}}
	job := domain.Job{Task: domain.TaskSpec{Artifact: domain.NativeArtifact{Architecture: "amd64", RuntimeRequirements: map[string]string{
		"os": "linux", "arch": "amd64", "libc.version": ">=2.17",
	}}, Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30}}}
	if !NodeFitsJob(node, job) {
		t.Fatal("satisfied runtime contract was rejected")
	}
	job.Task.Artifact.RuntimeRequirements["libc.version"] = ">=2.40"
	if NodeFitsJob(node, job) {
		t.Fatal("incompatible libc version was accepted")
	}
	delete(node.RuntimeFacts, "libc.version")
	job.Task.Artifact.RuntimeRequirements["libc.version"] = ">=2.17"
	if NodeFitsJob(node, job) {
		t.Fatal("missing required runtime fact was accepted")
	}
}

func TestSelectNodeUsesPreferredZoneAndHealthyNetworkCapacity(t *testing.T) {
	base := domain.Node{Status: domain.NodeStatusReady, Architecture: "amd64", Capabilities: []string{"cgroup-v2-executor-v1"}, Allocatable: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30}}
	east, west := base, base
	east.ID, east.Labels = "east", map[string]string{"topology.xirang.io/zone": "east"}
	west.ID, west.Labels = "west", map[string]string{"topology.xirang.io/zone": "west"}
	job := domain.Job{Task: domain.TaskSpec{Artifact: domain.NativeArtifact{Architecture: "amd64"}, Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30}, Placement: domain.Placement{PreferredZone: "east", MinimumNetworkBits: 10_000_000_000}}}
	snapshot := &domain.TopologySnapshot{Edges: []domain.TopologyEdge{
		{ID: "east-25g", SourceID: "east", TargetID: "switch", CapacityBits: 25_000_000_000, Health: "healthy"},
		{ID: "west-100g-bad", SourceID: "west", TargetID: "switch", CapacityBits: 100_000_000_000, Health: "critical"},
	}}
	selected, ok := selectNodeWithTopology(job, []domain.Node{west, east}, snapshot)
	if !ok || selected.ID != "east" {
		t.Fatalf("topology-aware selection failed: node=%#v ok=%t", selected, ok)
	}
}

func TestSelectNodeUsesAdminPriorityThenAutomaticWeight(t *testing.T) {
	base := domain.Node{
		Status: domain.NodeStatusReady, Architecture: "amd64",
		Capabilities: []string{"cgroup-v2-executor-v1"},
		Allocatable:  domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30},
	}
	slow, fast := base, base
	slow.ID, slow.ComputeWeight = "slow", 1
	fast.ID, fast.ComputeWeight = "fast", 8
	job := domain.Job{Task: domain.TaskSpec{
		Artifact:  domain.NativeArtifact{Architecture: "amd64"},
		Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30},
	}}
	selected, ok := selectNode(job, []domain.Node{slow, fast})
	if !ok || selected.ID != "fast" {
		t.Fatalf("automatic compute weight was ignored: %#v", selected)
	}
	slow.ComputePriority = 10
	selected, ok = selectNode(job, []domain.Node{slow, fast})
	if !ok || selected.ID != "slow" {
		t.Fatalf("administrator compute priority was ignored: %#v", selected)
	}
}

func TestTaskComputeWeightDoesNotSendCPUWorkToGPUNodeByAccident(t *testing.T) {
	base := domain.Node{Status: domain.NodeStatusReady, Architecture: "amd64", Capabilities: []string{"cgroup-v2-executor-v1", "device-resource-executor-v1", "device-resource-executor-v1/nvidia.com/gpu"}, Allocatable: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30, Devices: map[string]int64{"nvidia.com/gpu": 1}}}
	cpuFast, gpuHeavy := base, base
	cpuFast.ID, cpuFast.CPUBenchmarkScore, cpuFast.ComputeWeight = "cpu-fast", 12, 12
	gpuHeavy.ID, gpuHeavy.CPUBenchmarkScore, gpuHeavy.ComputeWeight = "gpu-heavy", 8, 80
	artifact := domain.NativeArtifact{Architecture: "amd64"}
	cpuJob := domain.Job{Task: domain.TaskSpec{Artifact: artifact, Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30}}}
	selected, ok := selectNode(cpuJob, []domain.Node{cpuFast, gpuHeavy})
	if !ok || selected.ID != "cpu-fast" {
		t.Fatalf("CPU-only task was biased by GPU capacity: %#v", selected)
	}
	gpuJob := cpuJob
	gpuJob.Task.Resources.Devices = map[string]int64{"nvidia.com/gpu": 1}
	selected, ok = selectNode(gpuJob, []domain.Node{cpuFast, gpuHeavy})
	if !ok || selected.ID != "gpu-heavy" {
		t.Fatalf("GPU task did not use aggregate GPU-aware weight: %#v", selected)
	}
}

func TestPlanBatchUsesApplicationMappingAndComputeWeights(t *testing.T) {
	base := domain.Node{Status: domain.NodeStatusReady, Architecture: "amd64", Capabilities: []string{"cgroup-v2-executor-v1"}, Allocatable: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30}}
	fast, slow, unmapped := base, base, base
	fast.ID, fast.CPUBenchmarkScore, fast.ComputeWeight = "fast", 3, 3
	slow.ID, slow.CPUBenchmarkScore, slow.ComputeWeight = "slow", 1, 1
	unmapped.ID, unmapped.CPUBenchmarkScore, unmapped.ComputeWeight = "unmapped", 100, 100
	artifact := domain.NativeArtifact{URI: "file:///shared/gmx", SHA256: strings.Repeat("a", 64), Architecture: "amd64", Entrypoint: "gmx"}
	task := domain.TaskSpec{Application: "gromacs", ArtifactVariants: []domain.NativeArtifactVariant{{BuildID: "build_gpu", Variant: "amd64-nvidia", NodeIDs: []string{"fast", "slow"}, Artifact: artifact}}, Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30}}
	coordinator := New(nil, batchNodeSource{fast, slow, unmapped}, nil)
	plan, err := coordinator.PlanBatch(context.Background(), task, 8, 3)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, nodeID := range plan {
		counts[nodeID]++
	}
	if len(plan) != 8 || counts["fast"] != 6 || counts["slow"] != 2 || counts["unmapped"] != 0 {
		t.Fatalf("unexpected weighted mapping plan: %#v", counts)
	}
	job := domain.Job{Task: task}
	message := assignmentCommand(job, "fast").GetRunAssignment()
	if message.GetArtifact().GetUri() != "file:///shared/gmx" {
		t.Fatalf("node-specific artifact was not dispatched: %#v", message.GetArtifact())
	}
}

func TestPlanBatchAcceptsWorkloadSpecificWeightModelWithSafeFallback(t *testing.T) {
	base := domain.Node{Status: domain.NodeStatusReady, Architecture: "amd64", Capabilities: []string{"cgroup-v2-executor-v1"}, Allocatable: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30}}
	fast, slow := base, base
	fast.ID, fast.CPUBenchmarkScore = "fast", 3
	slow.ID, slow.CPUBenchmarkScore = "slow", 1
	task := domain.TaskSpec{Artifact: domain.NativeArtifact{Architecture: "amd64"}, Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30}}
	coordinator := New(nil, batchNodeSource{fast, slow}, nil)
	coordinator.SetBatchWeightModel(BatchWeightFunc(func(node domain.Node, _ domain.TaskSpec) float64 {
		if node.ID == "slow" {
			return 3
		}
		return 1
	}))
	plan, err := coordinator.PlanBatch(context.Background(), task, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, nodeID := range plan {
		counts[nodeID]++
	}
	if counts["fast"] != 3 || counts["slow"] != 5 {
		t.Fatalf("custom batch weight model was ignored: %#v", counts)
	}

	coordinator.SetBatchWeightModel(BatchWeightFunc(func(domain.Node, domain.TaskSpec) float64 { return math.NaN() }))
	plan, err = coordinator.PlanBatch(context.Background(), task, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	clear(counts)
	for _, nodeID := range plan {
		counts[nodeID]++
	}
	if counts["fast"] != 6 || counts["slow"] != 2 {
		t.Fatalf("invalid custom weights did not fall back safely: %#v", counts)
	}
}

func TestNodeNetworkCapacityUsesHealthyPrimaryNotBandwidthSum(t *testing.T) {
	snapshot := &domain.TopologySnapshot{Edges: []domain.TopologyEdge{
		{ID: "healthy-10g", SourceID: "node", TargetID: "switch", CapacityBits: 10_000_000_000, Health: "healthy"},
		{ID: "degraded-25g", SourceID: "node", TargetID: "switch", CapacityBits: 25_000_000_000, Health: "degraded"},
		{ID: "critical-100g", SourceID: "node", TargetID: "switch", CapacityBits: 100_000_000_000, Health: "critical"},
	}}
	if capacity := nodeNetworkCapacity("node", snapshot); capacity != 10_000_000_000 {
		t.Fatalf("expected healthy 10G primary without aggregation, got %d", capacity)
	}
	snapshot.PathSelections = []domain.NetworkPathSelection{{SourceID: "node", TargetID: "switch", EffectiveCapacityBits: 25_000_000_000, Policy: "single-best-healthy"}}
	if capacity := nodeNetworkCapacity("node", snapshot); capacity != 25_000_000_000 {
		t.Fatalf("expected enriched path selection capacity, got %d", capacity)
	}
}

func TestTopologyPairIndexTreatsUnavailableLatencyAsWorst(t *testing.T) {
	snapshot := &domain.TopologySnapshot{Edges: []domain.TopologyEdge{
		{ID: "critical-fast", SourceID: "a", TargetID: "b", CapacityBits: 100_000_000_000, LatencyMicros: 10, Health: "critical"},
		{ID: "unknown", SourceID: "a", TargetID: "b", CapacityBits: 25_000_000_000, LatencyMicros: 0, Health: "healthy"},
		{ID: "measured", SourceID: "a", TargetID: "b", CapacityBits: 25_000_000_000, LatencyMicros: 400, Health: "healthy"},
	}}
	pair := buildTopologyPairIndex(snapshot)[topologyPairKey("a", "b")]
	if pair.edgeID != "measured" || pair.latency != 400 {
		t.Fatalf("unavailable latency incorrectly outranked a measurement: %#v", pair)
	}

	snapshot.PathSelections = []domain.NetworkPathSelection{{SourceID: "a", TargetID: "b", PrimaryEdgeID: "unknown"}}
	pair = buildTopologyPairIndex(snapshot)[topologyPairKey("a", "b")]
	if pair.edgeID != "unknown" || pair.latency != math.MaxInt64 {
		t.Fatalf("selected path did not normalize unavailable latency: %#v", pair)
	}
}

func TestSelectNodeSetBacktracksAroundHighBandwidthDeadEnds(t *testing.T) {
	base := domain.Node{
		Status: domain.NodeStatusReady, Architecture: "amd64",
		Capabilities: []string{"cgroup-v2-executor-v1", "gang-assignment-v1"},
		Allocatable:  domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30},
	}
	nodes := make([]domain.Node, 0, 6)
	for _, id := range []string{"a", "b", "c", "x", "y", "z"} {
		node := base
		node.ID = id
		nodes = append(nodes, node)
	}
	edges := []domain.TopologyEdge{
		{ID: "a-b", SourceID: "a", TargetID: "b", CapacityBits: 10_000_000_000, Health: "healthy"},
		{ID: "a-c", SourceID: "a", TargetID: "c", CapacityBits: 10_000_000_000, Health: "healthy"},
		{ID: "b-c", SourceID: "b", TargetID: "c", CapacityBits: 10_000_000_000, Health: "healthy"},
		{ID: "a-x", SourceID: "a", TargetID: "x", CapacityBits: 100_000_000_000, Health: "healthy"},
		{ID: "b-y", SourceID: "b", TargetID: "y", CapacityBits: 100_000_000_000, Health: "healthy"},
		{ID: "c-z", SourceID: "c", TargetID: "z", CapacityBits: 100_000_000_000, Health: "healthy"},
	}
	job := domain.Job{Task: domain.TaskSpec{
		Artifact:  domain.NativeArtifact{Architecture: "amd64"},
		Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30},
		Placement: domain.Placement{MinimumNetworkBits: 1_000_000_000},
	}}
	selected, ok := selectNodeSetWithTopology(job, nodes, 3, &domain.TopologySnapshot{Edges: edges})
	if !ok || len(selected) != 3 || selected[0].ID != "a" || selected[1].ID != "b" || selected[2].ID != "c" {
		t.Fatalf("bounded backtracking did not recover the feasible fabric: nodes=%#v ok=%t", selected, ok)
	}
}

func TestGangBacktrackingRanksBestSetAndReportsBudget(t *testing.T) {
	base := domain.Node{Status: domain.NodeStatusReady, Architecture: "amd64", Capabilities: []string{"cgroup-v2-executor-v1", "gang-assignment-v1"}, Allocatable: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30}}
	nodes := make([]domain.Node, 4)
	for index, id := range []string{"a", "b", "c", "d"} {
		nodes[index], nodes[index].ID = base, id
	}
	edges := []domain.TopologyEdge{
		{ID: "a-b", SourceID: "a", TargetID: "b", CapacityBits: 10, LatencyMicros: 10, Health: "healthy"},
		{ID: "a-c", SourceID: "a", TargetID: "c", CapacityBits: 20, LatencyMicros: 10, Health: "healthy"},
		{ID: "b-c", SourceID: "b", TargetID: "c", CapacityBits: 10, LatencyMicros: 10, Health: "healthy"},
		{ID: "a-d", SourceID: "a", TargetID: "d", CapacityBits: 20, LatencyMicros: 5, Health: "healthy"},
		{ID: "c-d", SourceID: "c", TargetID: "d", CapacityBits: 20, LatencyMicros: 5, Health: "healthy"},
	}
	job := domain.Job{Task: domain.TaskSpec{Artifact: domain.NativeArtifact{Architecture: "amd64"}, Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30}, Placement: domain.Placement{MinimumNetworkBits: 1}}}
	complete := selectNodeSetWithTopologyDetailed(job, nodes, 3, &domain.TopologySnapshot{Edges: edges}, 1_000)
	if complete.budgetExhausted || len(complete.nodes) != 3 || complete.nodes[0].ID != "a" || complete.nodes[1].ID != "c" || complete.nodes[2].ID != "d" {
		t.Fatalf("ranked search did not return exact best set: %#v", complete)
	}
	limited := selectNodeSetWithTopologyDetailed(job, nodes, 3, &domain.TopologySnapshot{Edges: edges}, 1)
	if !limited.budgetExhausted || limited.visited != 1 {
		t.Fatalf("search budget was not surfaced: %#v", limited)
	}
}

func TestNodeFitsJobEnforcesCPUAndDeviceNUMALocality(t *testing.T) {
	node := domain.Node{
		ID: "numa-node", Status: domain.NodeStatusReady, Architecture: "amd64",
		Capabilities:   []string{"cgroup-v2-executor-v1", "cpuset-affinity-v1", "device-resource-executor-v1", "device-resource-executor-v1/xirang.io/drm-render"},
		Allocatable:    domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30, Devices: map[string]int64{"xirang.io/drm-render": 2}},
		NUMATopology:   []domain.NUMANode{{ID: 0, CPUs: "0-3", MemoryBytes: 8 << 30}, {ID: 1, CPUs: "4-7", MemoryBytes: 8 << 30}},
		DeviceTopology: map[string]map[string]int64{"xirang.io/drm-render": {"0": 1, "1": 1}},
	}
	job := domain.Job{Task: domain.TaskSpec{
		Artifact:  domain.NativeArtifact{Architecture: "amd64"},
		Resources: domain.ResourceVector{MilliCPU: 1_000, MemoryBytes: 1 << 30, Devices: map[string]int64{"xirang.io/drm-render": 1}},
		Placement: domain.Placement{CPUSet: "0-1", NUMANodes: "0"},
	}}
	if !NodeFitsJob(node, job) {
		t.Fatal("valid CPU and device NUMA locality was rejected")
	}
	job.Task.Resources.Devices["xirang.io/drm-render"] = 2
	if NodeFitsJob(node, job) {
		t.Fatal("device request exceeding selected NUMA locality was accepted")
	}
	job.Task.Resources.Devices["xirang.io/drm-render"] = 1
	job.Task.Placement.CPUSet = "8"
	if NodeFitsJob(node, job) {
		t.Fatal("CPU set outside node NUMA topology was accepted")
	}
}
