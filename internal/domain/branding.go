package domain

import "time"

// ClusterBranding is public presentation data, including on the sign-in page.
// It does not change node identities, certificate names, or protocol identifiers.
type ClusterBranding struct {
	Name        string    `json:"name"`
	LogoDataURL string    `json:"logoDataUrl"`
	Revision    uint64    `json:"revision"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}
