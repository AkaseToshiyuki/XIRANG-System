package domain

import "time"

type HealthSource struct {
	Name          string `json:"name"`
	Status        string `json:"status"` // available, unavailable, disabled, partial
	Message       string `json:"message,omitempty"`
	WindowSeconds int64  `json:"windowSeconds,omitempty"`
	Limit         int    `json:"limit,omitempty"`
}

type HardwareSensor struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Source        string   `json:"source"`
	Value         float64  `json:"value"`
	Available     bool     `json:"available"`
	Unit          string   `json:"unit"`
	Status        string   `json:"status"`
	LowerCritical *float64 `json:"lowerCritical,omitempty"`
	UpperCritical *float64 `json:"upperCritical,omitempty"`
}

type SystemEvent struct {
	ID             string     `json:"id"`
	NodeID         string     `json:"nodeId"`
	Source         string     `json:"source"`
	SourceRecordID string     `json:"sourceRecordId,omitempty"`
	Kind           string     `json:"kind"` // error, corrected, recovery, repair, info
	Severity       string     `json:"severity"`
	Message        string     `json:"message"`
	Component      string     `json:"component,omitempty"`
	OccurredAt     *time.Time `json:"occurredAt,omitempty"`
	ReceivedAt     time.Time  `json:"receivedAt"`
	Actor          string     `json:"actor,omitempty"`
	RelatedEventID string     `json:"relatedEventId,omitempty"`
}

type NodeHealthSnapshot struct {
	NodeID            string           `json:"nodeId"`
	ObservedAt        time.Time        `json:"observedAt"`
	ReceivedAt        time.Time        `json:"receivedAt"`
	Sources           []HealthSource   `json:"sources"`
	Sensors           []HardwareSensor `json:"sensors"`
	ChassisPower      string           `json:"chassisPower"`
	IdentifySupported bool             `json:"identifySupported"`
}

type BMCAction struct {
	ID          string     `json:"id"`
	NodeID      string     `json:"nodeId"`
	Action      string     `json:"action"`
	Actor       string     `json:"actor"`
	Status      string     `json:"status"` // queued, succeeded, failed
	Message     string     `json:"message,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}
