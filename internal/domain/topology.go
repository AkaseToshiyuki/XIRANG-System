package domain

import "time"

type TopologyNodeKind string

const (
	TopologyNodeHost    TopologyNodeKind = "host"
	TopologyNodeSwitch  TopologyNodeKind = "switch"
	TopologyNodeRouter  TopologyNodeKind = "router"
	TopologyNodeUnknown TopologyNodeKind = "unknown"
)

type TopologyVertex struct {
	ID     string            `json:"id"`
	Kind   TopologyNodeKind  `json:"kind"`
	Labels map[string]string `json:"labels,omitempty"`
}

type TopologyEdge struct {
	ID                       string    `json:"id"`
	Type                     string    `json:"type"`
	SourceID                 string    `json:"sourceId"`
	TargetID                 string    `json:"targetId"`
	SourceInterface          string    `json:"sourceInterface,omitempty"`
	TargetInterface          string    `json:"targetInterface,omitempty"`
	SourceAddress            string    `json:"sourceAddress,omitempty"`
	TargetAddress            string    `json:"targetAddress,omitempty"`
	CapacityBits             int64     `json:"capacityBits,omitempty"`
	ObservedBits             int64     `json:"observedBits,omitempty"`
	ObservedPacketsPerSecond float64   `json:"observedPacketsPerSecond,omitempty"`
	DroppedPacketsPerSecond  float64   `json:"droppedPacketsPerSecond,omitempty"`
	ErrorPacketsPerSecond    float64   `json:"errorPacketsPerSecond,omitempty"`
	DropRatio                float64   `json:"dropRatio,omitempty"`
	Health                   string    `json:"health,omitempty"`
	LatencyMicros            int64     `json:"latencyMicros,omitempty"`
	ObservedAt               time.Time `json:"observedAt"`
}

// NetworkPathSelection describes the control-plane choice among parallel,
// independently observed links. XIRANG intentionally does not add their
// capacities unless an explicit multipath data plane is introduced.
type NetworkPathSelection struct {
	SourceID               string   `json:"sourceId"`
	TargetID               string   `json:"targetId"`
	PrimaryEdgeID          string   `json:"primaryEdgeId"`
	PrimaryReverseEdgeID   string   `json:"primaryReverseEdgeId,omitempty"`
	FallbackEdgeIDs        []string `json:"fallbackEdgeIds,omitempty"`
	FallbackReverseEdgeIDs []string `json:"fallbackReverseEdgeIds,omitempty"`
	EffectiveCapacityBits  int64    `json:"effectiveCapacityBits,omitempty"`
	EffectiveLatencyMicros int64    `json:"effectiveLatencyMicros,omitempty"`
	Health                 string   `json:"health,omitempty"`
	Bidirectional          bool     `json:"bidirectional"`
	Policy                 string   `json:"policy"`
}

type TopologyRoute struct {
	NodeID        string `json:"nodeId,omitempty"`
	Destination   string `json:"destination"`
	Gateway       string `json:"gateway,omitempty"`
	InterfaceName string `json:"interfaceName"`
	Metric        uint32 `json:"metric,omitempty"`
	Table         uint32 `json:"table"`
	Family        string `json:"family"`
}

type TopologyNeighbor struct {
	NodeID        string `json:"nodeId,omitempty"`
	Address       string `json:"address"`
	MACAddress    string `json:"macAddress,omitempty"`
	InterfaceName string `json:"interfaceName"`
	State         string `json:"state"`
	Router        bool   `json:"router,omitempty"`
}

type TopologyLLDPNeighbor struct {
	NodeID              string   `json:"nodeId,omitempty"`
	LocalInterface      string   `json:"localInterface"`
	ChassisID           string   `json:"chassisId"`
	PortID              string   `json:"portId,omitempty"`
	SystemName          string   `json:"systemName,omitempty"`
	SystemDescription   string   `json:"systemDescription,omitempty"`
	ManagementAddresses []string `json:"managementAddresses,omitempty"`
}

// NetworkPathProbe is one bounded source-address-to-target-address reachability
// measurement. It records declared interface capacities without treating them
// as measured throughput.
type NetworkPathProbe struct {
	NodeID                   string    `json:"nodeId,omitempty"`
	TargetNodeID             string    `json:"targetNodeId"`
	SourceInterface          string    `json:"sourceInterface"`
	SourceAddress            string    `json:"sourceAddress"`
	TargetInterface          string    `json:"targetInterface"`
	TargetAddress            string    `json:"targetAddress"`
	SourceSpeedBitsPerSecond uint64    `json:"sourceSpeedBitsPerSecond,omitempty"`
	TargetSpeedBitsPerSecond uint64    `json:"targetSpeedBitsPerSecond,omitempty"`
	LatencyMicros            int64     `json:"latencyMicros,omitempty"`
	Reachable                bool      `json:"reachable"`
	Failure                  string    `json:"failure,omitempty"`
	ObservedAt               time.Time `json:"observedAt"`
}

type NodeTopologyReport struct {
	MessageID     string                 `json:"messageId"`
	NodeID        string                 `json:"nodeId"`
	Routes        []TopologyRoute        `json:"routes,omitempty"`
	Neighbors     []TopologyNeighbor     `json:"neighbors,omitempty"`
	LLDPNeighbors []TopologyLLDPNeighbor `json:"lldpNeighbors,omitempty"`
	PathProbes    []NetworkPathProbe     `json:"pathProbes,omitempty"`
	ContentHash   string                 `json:"contentHash"`
	ObservedAt    time.Time              `json:"observedAt"`
	ReceivedAt    time.Time              `json:"receivedAt"`
}

type TopologySnapshot struct {
	Version        uint64                 `json:"version"`
	GeneratedAt    time.Time              `json:"generatedAt"`
	ReportCount    int                    `json:"reportCount"`
	Vertices       []TopologyVertex       `json:"vertices"`
	Edges          []TopologyEdge         `json:"edges"`
	PathSelections []NetworkPathSelection `json:"pathSelections,omitempty"`
	Routes         []TopologyRoute        `json:"routes,omitempty"`
	Neighbors      []TopologyNeighbor     `json:"neighbors,omitempty"`
	LLDP           []TopologyLLDPNeighbor `json:"lldpNeighbors,omitempty"`
	PathProbes     []NetworkPathProbe     `json:"pathProbes,omitempty"`
}
