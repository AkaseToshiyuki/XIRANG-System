package domain

import "time"

type NodeRole string

const (
	NodeRoleManagement NodeRole = "management"
	NodeRoleCompute    NodeRole = "compute"
	NodeRoleStorage    NodeRole = "storage"
)

type NodeStatus string

const (
	NodeStatusDiscovered      NodeStatus = "discovered"
	NodeStatusPendingApproval NodeStatus = "pending_approval"
	NodeStatusEnrolling       NodeStatus = "enrolling"
	NodeStatusReady           NodeStatus = "ready"
	NodeStatusCordoned        NodeStatus = "cordoned"
	NodeStatusDraining        NodeStatus = "draining"
	NodeStatusUnreachable     NodeStatus = "unreachable"
	NodeStatusOffline         NodeStatus = "offline"
	NodeStatusRejected        NodeStatus = "rejected"
	NodeStatusRemoved         NodeStatus = "removed"
)

// Taint prevents scheduling unless a workload declares a matching toleration.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

type NUMANode struct {
	ID          uint32 `json:"id"`
	CPUs        string `json:"cpus"`
	MemoryBytes int64  `json:"memoryBytes"`
}

// Node describes control-plane identity, scheduling capacity, and observed state.
type Node struct {
	ID                           string                      `json:"id"`
	Name                         string                      `json:"name"`
	Status                       NodeStatus                  `json:"status"`
	DesiredStatus                NodeStatus                  `json:"desiredStatus,omitempty"`
	PendingModeCommandID         string                      `json:"pendingModeCommandId,omitempty"`
	PendingManagementCommandID   string                      `json:"pendingManagementCommandId,omitempty"`
	DesiredManagementPanel       *bool                       `json:"desiredManagementPanel,omitempty"`
	ManagementPanelEnabled       bool                        `json:"managementPanelEnabled,omitempty"`
	ManagementPanelStatus        string                      `json:"managementPanelStatus,omitempty"`
	OS                           string                      `json:"os"`
	Architecture                 string                      `json:"architecture"`
	Distribution                 string                      `json:"distribution,omitempty"`
	KernelVersion                string                      `json:"kernelVersion,omitempty"`
	RuntimeFacts                 map[string]string           `json:"runtimeFacts,omitempty"`
	AgentVersion                 string                      `json:"agentVersion,omitempty"`
	Capabilities                 []string                    `json:"capabilities,omitempty"`
	CPUModel                     string                      `json:"cpuModel,omitempty"`
	CPUFeatures                  []string                    `json:"cpuFeatures,omitempty"`
	CPUBenchmarkScore            float64                     `json:"cpuBenchmarkScore,omitempty"`
	ComputeWeight                float64                     `json:"computeWeight,omitempty"`
	ComputePriority              int32                       `json:"computePriority,omitempty"`
	CertificateSerial            string                      `json:"certificateSerial,omitempty"`
	CertificateNotAfter          *time.Time                  `json:"certificateNotAfter,omitempty"`
	PendingCertificateSerial     string                      `json:"pendingCertificateSerial,omitempty"`
	PendingCertificateNotAfter   *time.Time                  `json:"pendingCertificateNotAfter,omitempty"`
	PendingCertificateCommandID  string                      `json:"pendingCertificateCommandId,omitempty"`
	PendingCertificateRotationID string                      `json:"pendingCertificateRotationId,omitempty"`
	CertificateRevokedAt         *time.Time                  `json:"certificateRevokedAt,omitempty"`
	RemovedAt                    *time.Time                  `json:"removedAt,omitempty"`
	Labels                       map[string]string           `json:"labels,omitempty"`
	Roles                        []NodeRole                  `json:"roles,omitempty"`
	ManagementAddress            string                      `json:"managementAddress,omitempty"`
	SSHUser                      string                      `json:"sshUser,omitempty"`
	Group                        string                      `json:"group,omitempty"`
	Taints                       []Taint                     `json:"taints,omitempty"`
	NUMATopology                 []NUMANode                  `json:"numaTopology,omitempty"`
	DeviceTopology               map[string]map[string]int64 `json:"deviceTopology,omitempty"`
	Capacity                     ResourceVector              `json:"capacity"`
	Allocatable                  ResourceVector              `json:"allocatable"`
	Allocated                    ResourceVector              `json:"allocated"`
	Revision                     uint64                      `json:"revision"`
	CreatedAt                    time.Time                   `json:"createdAt"`
	UpdatedAt                    time.Time                   `json:"updatedAt"`
	ApprovedAt                   *time.Time                  `json:"approvedAt,omitempty"`
	LastSeenAt                   time.Time                   `json:"lastSeenAt"`
}

// HasRole keeps nodes enrolled by earlier versions schedulable until an
// administrator explicitly assigns roles.
func (n Node) HasRole(role NodeRole) bool {
	if len(n.Roles) == 0 {
		return role == NodeRoleCompute
	}
	for _, candidate := range n.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

// Schedulable is intentionally strict: only Ready nodes receive new work.
func (n Node) Schedulable() bool {
	return n.HasRole(NodeRoleCompute) && n.Status == NodeStatusReady && (n.DesiredStatus == "" || n.DesiredStatus == NodeStatusReady)
}

// CanTransitionNode reports whether a requested node lifecycle transition is valid.
func CanTransitionNode(from, to NodeStatus) bool {
	allowed := map[NodeStatus]map[NodeStatus]bool{
		NodeStatusDiscovered:      {NodeStatusPendingApproval: true, NodeStatusRejected: true},
		NodeStatusPendingApproval: {NodeStatusEnrolling: true, NodeStatusReady: true, NodeStatusRejected: true},
		NodeStatusEnrolling:       {NodeStatusReady: true, NodeStatusRejected: true, NodeStatusUnreachable: true},
		NodeStatusReady:           {NodeStatusCordoned: true, NodeStatusUnreachable: true, NodeStatusOffline: true},
		NodeStatusCordoned:        {NodeStatusReady: true, NodeStatusDraining: true, NodeStatusUnreachable: true, NodeStatusOffline: true},
		NodeStatusDraining:        {NodeStatusCordoned: true, NodeStatusUnreachable: true, NodeStatusOffline: true},
		NodeStatusUnreachable:     {NodeStatusReady: true, NodeStatusCordoned: true, NodeStatusDraining: true, NodeStatusOffline: true},
		NodeStatusOffline:         {NodeStatusReady: true, NodeStatusCordoned: true, NodeStatusDraining: true},
	}
	return allowed[from][to]
}
