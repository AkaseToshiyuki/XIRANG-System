package domain

import "time"

type SharedSpaceTarget string

const (
	SharedSpaceAll   SharedSpaceTarget = "all"
	SharedSpaceGroup SharedSpaceTarget = "group"
	SharedSpaceNodes SharedSpaceTarget = "nodes"
)

type SharedSpaceNodeResult struct {
	NodeID  string `json:"nodeId"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type SharedFileOwner struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SizeBytes uint64 `json:"sizeBytes,omitempty"`
	Directory bool   `json:"directory,omitempty"`
}

type SharedFile struct {
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Directory   bool      `json:"directory"`
	SizeBytes   uint64    `json:"sizeBytes"`
	ModifiedAt  time.Time `json:"modifiedAt"`
	Owner       string    `json:"owner"`
	OwnedByUser bool      `json:"ownedByUser"`
}

type SharedSpace struct {
	ID                 string                     `json:"id"`
	Name               string                     `json:"name"`
	SourceNodeID       string                     `json:"sourceNodeId"`
	SourcePath         string                     `json:"sourcePath"`
	MountPath          string                     `json:"mountPath"`
	Target             SharedSpaceTarget          `json:"target"`
	TargetGroup        string                     `json:"targetGroup,omitempty"`
	TargetNodeIDs      []string                   `json:"targetNodeIds,omitempty"`
	Status             string                     `json:"status"`
	Results            []SharedSpaceNodeResult    `json:"results,omitempty"`
	UsedBytes          uint64                     `json:"usedBytes"`
	AvailableBytes     uint64                     `json:"availableBytes,omitempty"`
	TotalBytes         uint64                     `json:"totalBytes,omitempty"`
	CapacityObservedAt *time.Time                 `json:"capacityObservedAt,omitempty"`
	CapacityError      string                     `json:"capacityError,omitempty"`
	FileOwners         map[string]SharedFileOwner `json:"fileOwners,omitempty"`
	CreatedAt          time.Time                  `json:"createdAt"`
	UpdatedAt          time.Time                  `json:"updatedAt"`
}
