package domain

import "time"

const (
	DefaultMaxJobTimeoutSeconds   int64 = 7 * 24 * 60 * 60
	MaxSupportedJobTimeoutSeconds int64 = int64((1<<63 - 1) / time.Second)
)

type UserStatus string

const (
	UserStatusPending  UserStatus = "pending"
	UserStatusActive   UserStatus = "active"
	UserStatusRejected UserStatus = "rejected"
	UserStatusDisabled UserStatus = "disabled"
)

type User struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName"`
	Role        string     `json:"role"`
	GroupIDs    []string   `json:"groupIds,omitempty"`
	Status      UserStatus `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ApprovedAt  *time.Time `json:"approvedAt,omitempty"`
	ApprovedBy  string     `json:"approvedBy,omitempty"`
}

type UserGroup struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	ResourceQuota        ResourceVector `json:"resourceQuota"`
	StorageQuotaBytes    int64          `json:"storageQuotaBytes"`
	MaxConcurrentJobs    int32          `json:"maxConcurrentJobs"`
	MaxJobTimeoutSeconds int64          `json:"maxJobTimeoutSeconds"`
	Disabled             bool           `json:"disabled,omitempty"`
	DisableSuspendQueued bool           `json:"disableSuspendQueued,omitempty"`
	DisableEndSessions   bool           `json:"disableEndSessions,omitempty"`
	CreatedAt            time.Time      `json:"createdAt"`
	UpdatedAt            time.Time      `json:"updatedAt"`
}
