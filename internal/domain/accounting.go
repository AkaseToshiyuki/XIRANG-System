package domain

import "time"

// DailyUsage is a controller-owned aggregate of cumulative Agent counters.
// Raw units are retained so changing display precision never changes evidence.
type DailyUsage struct {
	Date                 string `json:"date"`
	OwnerID              string `json:"ownerId"`
	OwnerName            string `json:"ownerName,omitempty"`
	CPUUsageMicros       uint64 `json:"cpuUsageMicros"`
	MemoryByteSeconds    uint64 `json:"memoryByteSeconds"`
	GPUDeviceSeconds     uint64 `json:"gpuDeviceSeconds"`
	WallTimeMillis       uint64 `json:"wallTimeMillis"`
	WorkspaceByteSeconds uint64 `json:"workspaceByteSeconds"`
	PeakWorkspaceBytes   uint64 `json:"peakWorkspaceBytes"`
}

// BillingRate is immutable once created. A replacement is a new version with
// a later EffectiveAt timestamp, preserving historical invoice reproducibility.
type BillingRate struct {
	ID                  string    `json:"id"`
	Currency            string    `json:"currency"`
	CPUCoreHourMicros   int64     `json:"cpuCoreHourMicros"`
	MemoryGiBHourMicros int64     `json:"memoryGiBHourMicros"`
	GPUDeviceHourMicros int64     `json:"gpuDeviceHourMicros"`
	StorageGiBDayMicros int64     `json:"storageGiBDayMicros"`
	EffectiveAt         time.Time `json:"effectiveAt"`
	CreatedAt           time.Time `json:"createdAt"`
	CreatedBy           string    `json:"createdBy"`
}

type UsageCost struct {
	CPUCostMicros     int64 `json:"cpuCostMicros"`
	MemoryCostMicros  int64 `json:"memoryCostMicros"`
	GPUCostMicros     int64 `json:"gpuCostMicros"`
	StorageCostMicros int64 `json:"storageCostMicros"`
	TotalCostMicros   int64 `json:"totalCostMicros"`
}

type DailyUsageCost struct {
	DailyUsage
	Cost UsageCost `json:"cost"`
}

type UsageSummary struct {
	OwnerID              string           `json:"ownerId,omitempty"`
	OwnerName            string           `json:"ownerName,omitempty"`
	From                 string           `json:"from"`
	To                   string           `json:"to"`
	Currency             string           `json:"currency"`
	CPUUsageMicros       uint64           `json:"cpuUsageMicros"`
	MemoryByteSeconds    uint64           `json:"memoryByteSeconds"`
	GPUDeviceSeconds     uint64           `json:"gpuDeviceSeconds"`
	WallTimeMillis       uint64           `json:"wallTimeMillis"`
	WorkspaceByteSeconds uint64           `json:"workspaceByteSeconds"`
	PeakWorkspaceBytes   uint64           `json:"peakWorkspaceBytes"`
	Cost                 UsageCost        `json:"cost"`
	Daily                []DailyUsageCost `json:"daily"`
}
