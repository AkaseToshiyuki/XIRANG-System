package domain

import "time"

type AssignmentState string

const (
	AssignmentStateAccepted  AssignmentState = "accepted"
	AssignmentStatePulling   AssignmentState = "pulling"
	AssignmentStateReady     AssignmentState = "ready"
	AssignmentStateRunning   AssignmentState = "running"
	AssignmentStateSuspended AssignmentState = "suspended"
	AssignmentStateSucceeded AssignmentState = "succeeded"
	AssignmentStateFailed    AssignmentState = "failed"
	AssignmentStateCanceled  AssignmentState = "canceled"
)

type AssignmentReport struct {
	NodeID        string          `json:"nodeId"`
	AssignmentID  string          `json:"assignmentId"`
	Attempt       uint32          `json:"attempt"`
	State         AssignmentState `json:"state"`
	ExitCode      int32           `json:"exitCode,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	StdoutTail    string          `json:"stdoutTail,omitempty"`
	StderrTail    string          `json:"stderrTail,omitempty"`
	LogsTruncated bool            `json:"logsTruncated,omitempty"`
	OccurredAt    time.Time       `json:"occurredAt"`
	ReceivedAt    time.Time       `json:"receivedAt"`
}

type AssignmentUsageSnapshot struct {
	NodeID                   string    `json:"nodeId"`
	AssignmentID             string    `json:"assignmentId"`
	Attempt                  uint32    `json:"attempt"`
	CPUUsageMicros           uint64    `json:"cpuUsageMicros"`
	MemoryByteSeconds        uint64    `json:"memoryByteSeconds"`
	GPUDeviceSeconds         uint64    `json:"gpuDeviceSeconds"`
	WallTimeMillis           uint64    `json:"wallTimeMillis"`
	WorkspaceBytes           uint64    `json:"workspaceBytes"`
	FilesystemAvailableBytes uint64    `json:"filesystemAvailableBytes"`
	FilesystemTotalBytes     uint64    `json:"filesystemTotalBytes"`
	ObservedAt               time.Time `json:"observedAt"`
	ReceivedAt               time.Time `json:"receivedAt"`
}

type AssignmentLogSnapshot struct {
	NodeID        string    `json:"nodeId"`
	AssignmentID  string    `json:"assignmentId"`
	Attempt       uint32    `json:"attempt"`
	StdoutTail    string    `json:"stdoutTail,omitempty"`
	StderrTail    string    `json:"stderrTail,omitempty"`
	LogsTruncated bool      `json:"logsTruncated,omitempty"`
	ObservedAt    time.Time `json:"observedAt"`
	ReceivedAt    time.Time `json:"receivedAt"`
}

func (state AssignmentState) Terminal() bool {
	return state == AssignmentStateSucceeded || state == AssignmentStateFailed || state == AssignmentStateCanceled
}

func AssignmentProgress(state AssignmentState) int {
	switch state {
	case AssignmentStateAccepted:
		return 1
	case AssignmentStatePulling:
		return 2
	case AssignmentStateReady:
		return 3
	case AssignmentStateRunning, AssignmentStateSuspended:
		return 4
	case AssignmentStateSucceeded, AssignmentStateFailed, AssignmentStateCanceled:
		return 5
	default:
		return 0
	}
}
