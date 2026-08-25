package domain

import "time"

// JobTemplate is an administrator-curated, validated XIRANG 1 script that
// users can load into the normal editable job submission workflow.
type JobTemplate struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Script      string    `json:"script"`
	CreatedBy   string    `json:"createdBy"`
	UpdatedBy   string    `json:"updatedBy"`
	Revision    uint64    `json:"revision"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
