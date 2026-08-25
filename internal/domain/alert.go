package domain

import "time"

type AlertStatus string

const (
	AlertStatusPending      AlertStatus = "pending"
	AlertStatusFiring       AlertStatus = "firing"
	AlertStatusAcknowledged AlertStatus = "acknowledged"
	AlertStatusResolved     AlertStatus = "resolved"
)

type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

type Alert struct {
	ID                      string        `json:"id"`
	Fingerprint             string        `json:"fingerprint"`
	Kind                    string        `json:"kind"`
	SourceNodeID            string        `json:"sourceNodeId"`
	SourceInterface         string        `json:"sourceInterface"`
	Status                  AlertStatus   `json:"status"`
	Severity                AlertSeverity `json:"severity"`
	ObservedHealth          string        `json:"observedHealth"`
	Summary                 string        `json:"summary"`
	ObservedBits            int64         `json:"observedBits"`
	DroppedPacketsPerSecond float64       `json:"droppedPacketsPerSecond"`
	ErrorPacketsPerSecond   float64       `json:"errorPacketsPerSecond"`
	DropRatio               float64       `json:"dropRatio"`
	FirstObservedAt         time.Time     `json:"firstObservedAt"`
	LastObservedAt          time.Time     `json:"lastObservedAt"`
	FiringAt                *time.Time    `json:"firingAt,omitempty"`
	HealthySince            *time.Time    `json:"healthySince,omitempty"`
	AcknowledgedAt          *time.Time    `json:"acknowledgedAt,omitempty"`
	AcknowledgedBy          string        `json:"acknowledgedBy,omitempty"`
	ResolvedAt              *time.Time    `json:"resolvedAt,omitempty"`
	Revision                uint64        `json:"revision"`
}
