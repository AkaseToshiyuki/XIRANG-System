package domain

import "time"

// EnrollmentToken is stored with only a digest of the presented secret.
type EnrollmentToken struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	SecretHash      string     `json:"-"`
	RequireApproval bool       `json:"requireApproval"`
	MaxUses         uint32     `json:"maxUses"`
	Uses            uint32     `json:"uses"`
	CreatedAt       time.Time  `json:"createdAt"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	RevokedAt       *time.Time `json:"revokedAt,omitempty"`
}

func (token EnrollmentToken) Available(now time.Time) bool {
	return token.RevokedAt == nil && now.Before(token.ExpiresAt) && token.Uses < token.MaxUses
}

// InventorySummary is the Phase 1 portable subset reported during enrollment.
type InventorySummary struct {
	Hostname              string                      `json:"hostname"`
	OS                    string                      `json:"os"`
	Architecture          string                      `json:"architecture"`
	Distribution          string                      `json:"distribution,omitempty"`
	KernelVersion         string                      `json:"kernelVersion,omitempty"`
	AgentVersion          string                      `json:"agentVersion,omitempty"`
	CPUCount              int                         `json:"cpuCount"`
	CPUModel              string                      `json:"cpuModel,omitempty"`
	CPUFeatures           []string                    `json:"cpuFeatures,omitempty"`
	CPUBenchmarkScore     float64                     `json:"cpuBenchmarkScore,omitempty"`
	ComputeWeight         float64                     `json:"computeWeight,omitempty"`
	MemoryBytes           int64                       `json:"memoryBytes"`
	EphemeralStorageBytes int64                       `json:"ephemeralStorageBytes,omitempty"`
	Devices               map[string]int64            `json:"devices,omitempty"`
	NUMATopology          []NUMANode                  `json:"numaTopology,omitempty"`
	DeviceTopology        map[string]map[string]int64 `json:"deviceTopology,omitempty"`
}
