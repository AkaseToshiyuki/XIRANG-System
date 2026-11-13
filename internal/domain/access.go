package domain

import "time"

type Role struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Subject    string     `json:"subject"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	UserID     string     `json:"userId,omitempty"`
	// Interactive sessions can authenticate multiple requests. API tokens are
	// consumed on their first successful authentication, recorded in LastUsedAt.
	Interactive bool `json:"interactive,omitempty"`
	// Empty on older password sessions. Token-derived sessions retain their
	// original permission ceiling even if the owning account is promoted.
	AuthMethod        string   `json:"authMethod,omitempty"`
	PermissionCeiling []string `json:"permissionCeiling,omitempty"`
}

type Principal struct {
	UserID      string   `json:"userId,omitempty"`
	Subject     string   `json:"subject"`
	DisplayName string   `json:"displayName,omitempty"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
	AuthMethod  string   `json:"authMethod"`
	GroupIDs    []string `json:"groupIds,omitempty"`
}

// AccessSettings contains cluster-wide identity defaults. An empty default
// group keeps newly registered XIRANG accounts unassigned until an
// administrator explicitly selects one.
type AccessSettings struct {
	DefaultUserGroupID string    `json:"defaultUserGroupId"`
	UpdatedAt          time.Time `json:"updatedAt,omitempty"`
}
