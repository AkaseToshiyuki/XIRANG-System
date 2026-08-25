package domain

import "time"

type AlertDeliveryState string

const (
	AlertDeliveryPending   AlertDeliveryState = "pending"
	AlertDeliveryRetrying  AlertDeliveryState = "retrying"
	AlertDeliveryDelivered AlertDeliveryState = "delivered"
	AlertDeliveryFailed    AlertDeliveryState = "failed"
)

type AlertDelivery struct {
	ID             string             `json:"id"`
	EventType      string             `json:"eventType"`
	AlertID        string             `json:"alertId"`
	AlertRevision  uint64             `json:"alertRevision"`
	Alert          Alert              `json:"alert"`
	State          AlertDeliveryState `json:"state"`
	Attempts       int                `json:"attempts"`
	ResponseStatus int                `json:"responseStatus,omitempty"`
	LastError      string             `json:"lastError,omitempty"`
	CreatedAt      time.Time          `json:"createdAt"`
	NextAttemptAt  *time.Time         `json:"nextAttemptAt,omitempty"`
	LastAttemptAt  *time.Time         `json:"lastAttemptAt,omitempty"`
	DeliveredAt    *time.Time         `json:"deliveredAt,omitempty"`
}

type AlertWebhookStatus struct {
	Enabled     bool   `json:"enabled"`
	Destination string `json:"destination,omitempty"`
}
