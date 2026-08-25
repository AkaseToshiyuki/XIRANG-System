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
