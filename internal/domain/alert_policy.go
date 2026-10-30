package domain

import "time"

// AlertPolicy contains operator policy only. Webhook credentials and destinations
// remain host configuration and are never accepted through this API.
type AlertPolicy struct {
	TriggerAfterSeconds   float64       `json:"triggerAfterSeconds"`
	ResolveAfterSeconds   float64       `json:"resolveAfterSeconds"`
	RouteEnabled          bool          `json:"routeEnabled"`
	MinimumSeverity       AlertSeverity `json:"minimumSeverity"`
	Kinds                 []string      `json:"kinds"`
	SilenceUntil          *time.Time    `json:"silenceUntil,omitempty"`
	DeliveryTargetSeconds int           `json:"deliveryTargetSeconds"`
	Revision              uint64        `json:"revision"`
	UpdatedAt             time.Time     `json:"updatedAt,omitempty"`
}
