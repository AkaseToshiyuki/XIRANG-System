package domain

import "time"

type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusQueued    JobStatus = "queued"
	JobStatusAssigned  JobStatus = "assigned"
	JobStatusRunning   JobStatus = "running"
	JobStatusSuspended JobStatus = "suspended"
	JobStatusCanceling JobStatus = "canceling"
	JobStatusOrphaned  JobStatus = "orphaned"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCanceled  JobStatus = "canceled"
)

type Placement struct {
	RequiredNodeID       string            `json:"requiredNodeId,omitempty"`
	RequiredArchitecture string            `json:"requiredArchitecture,omitempty"`
	RequiredLabels       map[string]string `json:"requiredLabels,omitempty"`
	PreferredZone        string            `json:"preferredZone,omitempty"`
	RequiredTopology     map[string]string `json:"requiredTopology,omitempty"`
	ExcludedNodeIDs      []string          `json:"excludedNodeIds,omitempty"`
	Tolerations          []Toleration      `json:"tolerations,omitempty"`
	MinimumNetworkBits   int64             `json:"minimumNetworkBits,omitempty"`
	CPUSet               string            `json:"cpuSet,omitempty"`
	NUMANodes            string            `json:"numaNodes,omitempty"`
}

type Toleration struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

type TaskSpec struct {
	Artifact         NativeArtifact          `json:"artifact"`
	Application      string                  `json:"application,omitempty"`
	ArtifactVariants []NativeArtifactVariant `json:"artifactVariants,omitempty"`
	Arguments        []string                `json:"arguments,omitempty"`
	WorkingDirectory string                  `json:"workingDirectory,omitempty"`
	User             string                  `json:"user,omitempty"`
	Group            string                  `json:"group,omitempty"`
	Environment      map[string]string       `json:"environment,omitempty"`
	Resources        ResourceVector          `json:"resources"`
	Placement        Placement               `json:"placement,omitempty"`
	Parallel         ParallelSpec            `json:"parallel,omitempty"`
	Timeout          time.Duration           `json:"timeout,omitempty"`
}

// ParallelSpec is optional. Its zero value is the historical one-node,
// one-process native assignment so persisted jobs remain compatible with R3.
type ParallelSpec struct {
	Nodes        int32  `json:"nodes,omitempty"`
	TasksPerNode int32  `json:"tasksPerNode,omitempty"`
	Launcher     string `json:"launcher,omitempty"`
}

func (spec ParallelSpec) Effective() ParallelSpec {
	if spec.Nodes == 0 {
		spec.Nodes = 1
	}
	if spec.TasksPerNode == 0 {
		spec.TasksPerNode = 1
	}
	if spec.Launcher == "" {
		spec.Launcher = "native"
	}
	return spec
}

func (spec ParallelSpec) WorldSize() int32 {
	effective := spec.Effective()
	return effective.Nodes * effective.TasksPerNode
}

type AllocationState string

const (
	AllocationStatePreparing AllocationState = "preparing"
	AllocationStateReady     AllocationState = "ready"
	AllocationStateRunning   AllocationState = "running"
	AllocationStateCanceling AllocationState = "canceling"
	AllocationStateSucceeded AllocationState = "succeeded"
	AllocationStateFailed    AllocationState = "failed"
	AllocationStateCanceled  AllocationState = "canceled"
)

type AllocationHost struct {
	NodeID           string `json:"nodeId"`
	Hostname         string `json:"hostname"`
	Address          string `json:"address,omitempty"`
	NetworkInterface string `json:"networkInterface,omitempty"`
	Slots            int32  `json:"slots"`
}

type AllocationMember struct {
	NodeID           string          `json:"nodeId"`
	AssignmentID     string          `json:"assignmentId"`
	NodeRank         int32           `json:"nodeRank"`
	RankStart        int32           `json:"rankStart"`
	RankCount        int32           `json:"rankCount"`
	NetworkInterface string          `json:"networkInterface,omitempty"`
	Address          string          `json:"address,omitempty"`
	State            AssignmentState `json:"state,omitempty"`
	ExitCode         int32           `json:"exitCode,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	StdoutTail       string          `json:"stdoutTail,omitempty"`
	StderrTail       string          `json:"stderrTail,omitempty"`
	LogsTruncated    bool            `json:"logsTruncated,omitempty"`
	Usage            JobUsage        `json:"usage,omitempty"`
	UsageCheckpoint  JobUsage        `json:"usageCheckpoint,omitempty"`
	ObservedAt       *time.Time      `json:"observedAt,omitempty"`
	LogsObservedAt   *time.Time      `json:"logsObservedAt,omitempty"`
}

type RankAssignment struct {
	Rank      int32  `json:"rank"`
	LocalRank int32  `json:"localRank"`
	NodeRank  int32  `json:"nodeRank"`
	NodeID    string `json:"nodeId"`
}

type TaskGroup struct {
	ID       string           `json:"id"`
	Launcher string           `json:"launcher"`
	Ranks    []RankAssignment `json:"ranks"`
}

type Allocation struct {
	ID                    string             `json:"id"`
	Attempt               uint32             `json:"attempt"`
	State                 AllocationState    `json:"state"`
	WorldSize             int32              `json:"worldSize"`
	Launcher              string             `json:"launcher"`
	Members               []AllocationMember `json:"members"`
	Hosts                 []AllocationHost   `json:"hosts"`
	TaskGroups            []TaskGroup        `json:"taskGroups"`
	SelectionOptimal      bool               `json:"selectionOptimal"`
	SelectionSearchStates int                `json:"selectionSearchStates,omitempty"`
	CreatedAt             time.Time          `json:"createdAt"`
	UpdatedAt             time.Time          `json:"updatedAt"`
}

func (allocation *Allocation) MemberByAssignment(id string) (*AllocationMember, bool) {
	if allocation == nil {
		return nil, false
	}
	for index := range allocation.Members {
		if allocation.Members[index].AssignmentID == id {
			return &allocation.Members[index], true
		}
	}
	return nil, false
}

type JobDependency struct {
	JobID     string `json:"jobId"`
	Condition string `json:"condition"`
}

type NativeArtifact struct {
	URI                 string            `json:"uri"`
	SHA256              string            `json:"sha256"`
	Architecture        string            `json:"architecture"`
	ABI                 string            `json:"abi,omitempty"`
	Entrypoint          string            `json:"entrypoint"`
	Signature           []byte            `json:"signature,omitempty"`
	RuntimeRequirements map[string]string `json:"runtimeRequirements,omitempty"`
}

// NativeArtifactVariant binds one immutable build output to the compute nodes
// selected for that application variant. Jobs capture this routing snapshot at
// submission time, so later administrative changes affect only new jobs.
type NativeArtifactVariant struct {
	BuildID  string         `json:"buildId"`
	Variant  string         `json:"variant"`
	NodeIDs  []string       `json:"nodeIds"`
	Artifact NativeArtifact `json:"artifact"`
}

// JobBlockReason is a durable, machine-readable explanation for a job that is
// waiting in the scheduler. The historical Reason string remains available for
// terminal outcomes and old clients; new clients should prefer this structure.
type JobBlockReason struct {
	Code       string            `json:"code"`
	Category   string            `json:"category"`
	Message    string            `json:"message"`
	Details    map[string]string `json:"details,omitempty"`
	ObservedAt time.Time         `json:"observedAt"`
}

type Job struct {
	ID                      string          `json:"id"`
	OwnerID                 string          `json:"ownerId"`
	OwnerName               string          `json:"ownerName,omitempty"`
	GroupID                 string          `json:"groupId,omitempty"`
	PolicyGroupIDs          []string        `json:"policyGroupIds,omitempty"`
	SuspendedPolicyGroupIDs []string        `json:"suspendedPolicyGroupIds,omitempty"`
	StorageQuotaBytes       int64           `json:"storageQuotaBytes,omitempty"`
	ArrayID                 string          `json:"arrayId,omitempty"`
	ArrayIndex              int32           `json:"arrayIndex,omitempty"`
	ArraySize               int32           `json:"arraySize,omitempty"`
	Dependencies            []JobDependency `json:"dependencies,omitempty"`
	ProjectID               string          `json:"projectId"`
	Queue                   string          `json:"queue"`
	Priority                int32           `json:"priority"`
	Status                  JobStatus       `json:"status"`
	Task                    TaskSpec        `json:"task"`
	MaxAttempts             int32           `json:"maxAttempts"`
	Attempt                 uint32          `json:"attempt"`
	AssignmentID            string          `json:"assignmentId,omitempty"`
	NodeID                  string          `json:"nodeId,omitempty"`
	Allocation              *Allocation     `json:"allocation,omitempty"`
	ExitCode                int32           `json:"exitCode,omitempty"`
	Reason                  string          `json:"reason,omitempty"`
	BlockReason             *JobBlockReason `json:"blockReason,omitempty"`
	StdoutTail              string          `json:"stdoutTail,omitempty"`
	StderrTail              string          `json:"stderrTail,omitempty"`
	LogsTruncated           bool            `json:"logsTruncated,omitempty"`
	LogsObservedAt          *time.Time      `json:"logsObservedAt,omitempty"`
	Usage                   JobUsage        `json:"usage"`
	UsageCheckpoint         JobUsage        `json:"usageCheckpoint,omitempty"`
	CancelGracePeriod       time.Duration   `json:"cancelGracePeriod,omitempty"`
	PreemptedByJobID        string          `json:"preemptedByJobId,omitempty"`
	PreemptionCount         uint32          `json:"preemptionCount,omitempty"`
	PreemptionNodeID        string          `json:"preemptionNodeId,omitempty"`
	PreemptionVictims       []string        `json:"preemptionVictims,omitempty"`
	CreatedAt               time.Time       `json:"createdAt"`
	UpdatedAt               time.Time       `json:"updatedAt"`
	StartedAt               *time.Time      `json:"startedAt,omitempty"`
	FinishedAt              *time.Time      `json:"finishedAt,omitempty"`
}

type JobUsage struct {
	CPUUsageMicros           uint64     `json:"cpuUsageMicros"`
	MemoryByteSeconds        uint64     `json:"memoryByteSeconds"`
	GPUDeviceSeconds         uint64     `json:"gpuDeviceSeconds"`
	WallTimeMillis           uint64     `json:"wallTimeMillis"`
	WorkspaceBytes           uint64     `json:"workspaceBytes"`
	PeakWorkspaceBytes       uint64     `json:"peakWorkspaceBytes"`
	FilesystemAvailableBytes uint64     `json:"filesystemAvailableBytes"`
	FilesystemTotalBytes     uint64     `json:"filesystemTotalBytes"`
	ObservedAt               *time.Time `json:"observedAt,omitempty"`
}

func (s JobStatus) Terminal() bool {
	return s == JobStatusSucceeded || s == JobStatusFailed || s == JobStatusCanceled
}
