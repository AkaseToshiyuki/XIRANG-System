package domain

import "time"

type AuditEvent struct {
	ID         string            `json:"id"`
	ActorType  string            `json:"actorType"`
	ActorID    string            `json:"actorId"`
	Action     string            `json:"action"`
	TargetKind string            `json:"targetKind"`
	TargetID   string            `json:"targetId"`
	Outcome    string            `json:"outcome"`
	OccurredAt time.Time         `json:"occurredAt"`
	Details    map[string]string `json:"details,omitempty"`
}
