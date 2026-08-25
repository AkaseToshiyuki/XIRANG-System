package domain

import (
	"errors"
	"fmt"
)

// ResourceVector is the scheduler's canonical consumable-resource representation.
// CPU is expressed in millicores; byte values always use base-2 byte counts.
type ResourceVector struct {
	MilliCPU              int64            `json:"milliCpu"`
	MemoryBytes           int64            `json:"memoryBytes"`
	EphemeralStorageBytes int64            `json:"ephemeralStorageBytes"`
	Devices               map[string]int64 `json:"devices,omitempty"`
}

// Validate rejects values that cannot represent a schedulable resource quantity.
func (r ResourceVector) Validate() error {
	if r.MilliCPU < 0 || r.MemoryBytes < 0 || r.EphemeralStorageBytes < 0 {
		return errors.New("resource quantities cannot be negative")
	}
	for name, quantity := range r.Devices {
		if name == "" {
			return errors.New("device resource name cannot be empty")
		}
		if quantity < 0 {
			return fmt.Errorf("device %q quantity cannot be negative", name)
		}
	}
	return nil
}

// Fits reports whether available can satisfy requested without overcommit.
func (available ResourceVector) Fits(requested ResourceVector) bool {
	if available.MilliCPU < requested.MilliCPU ||
		available.MemoryBytes < requested.MemoryBytes ||
		available.EphemeralStorageBytes < requested.EphemeralStorageBytes {
		return false
	}
	for name, quantity := range requested.Devices {
		if available.Devices[name] < quantity {
			return false
		}
	}
	return true
}

// Subtract returns remaining resources and refuses an underflow.
func (available ResourceVector) Subtract(requested ResourceVector) (ResourceVector, error) {
	if !available.Fits(requested) {
		return ResourceVector{}, errors.New("requested resources exceed available resources")
	}
	remaining := ResourceVector{
		MilliCPU:              available.MilliCPU - requested.MilliCPU,
		MemoryBytes:           available.MemoryBytes - requested.MemoryBytes,
		EphemeralStorageBytes: available.EphemeralStorageBytes - requested.EphemeralStorageBytes,
		Devices:               make(map[string]int64, len(available.Devices)),
	}
	for name, quantity := range available.Devices {
		remaining.Devices[name] = quantity - requested.Devices[name]
	}
	return remaining, nil
}

// Add combines reservations. Callers must validate inputs before persistence.
func (r ResourceVector) Add(other ResourceVector) ResourceVector {
	combined := ResourceVector{
		MilliCPU:              r.MilliCPU + other.MilliCPU,
		MemoryBytes:           r.MemoryBytes + other.MemoryBytes,
		EphemeralStorageBytes: r.EphemeralStorageBytes + other.EphemeralStorageBytes,
		Devices:               make(map[string]int64, len(r.Devices)+len(other.Devices)),
	}
	for name, quantity := range r.Devices {
		combined.Devices[name] = quantity
	}
	for name, quantity := range other.Devices {
		combined.Devices[name] += quantity
	}
	if len(combined.Devices) == 0 {
		combined.Devices = nil
	}
	return combined
}
