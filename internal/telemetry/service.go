package telemetry

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/runtimecompat"
)

var (
	ErrNotFound      = errors.New("telemetry not found")
	ErrInvalidReport = errors.New("invalid telemetry report")
)

type Repository interface {
	PutInventory(context.Context, domain.NodeInventorySnapshot) error
	GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error)
	AppendObservation(context.Context, domain.NodeResourceObservation) error
	ListObservations(context.Context, string, int) ([]domain.NodeResourceObservation, error)
	Close() error
}

type Service struct {
	repository Repository
	now        func() time.Time
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository, now: time.Now}
}

func (service *Service) ReportInventory(ctx context.Context, nodeID string, observedAt time.Time, snapshot *xirangv1.InventorySnapshot) error {
	if !validIdentifier(nodeID, 256) || snapshot == nil || !validIdentifier(snapshot.Hostname, 256) ||
		!validIdentifier(snapshot.Architecture, 64) || snapshot.LogicalCpuCount == 0 || snapshot.MemoryBytes == 0 ||
		!validText(snapshot.Distribution, 512) || !validText(snapshot.KernelVersion, 256) ||
		!validText(snapshot.CpuModel, 512) || len(snapshot.CpuFeatures) > 1024 || !validTextSlice(snapshot.CpuFeatures, 64) ||
		!validMetric(snapshot.CpuBenchmarkScore, 0, 1_000_000) || !validMetric(snapshot.ComputeWeight, 0, 1_000_000) ||
		len(snapshot.NetworkInterfaces) > 256 || len(snapshot.Devices) > 256 || len(snapshot.NumaNodes) > 1024 || !validTextMap(snapshot.Labels, 256, 256, 1024) || runtimecompat.ValidateFacts(snapshot.RuntimeFacts) != nil ||
		invalidObservedAt(observedAt, service.now()) {
		return ErrInvalidReport
	}
	inventory := domain.NodeInventorySnapshot{
		NodeID: nodeID, Hostname: snapshot.Hostname, Distribution: snapshot.Distribution,
		KernelVersion: snapshot.KernelVersion, Architecture: snapshot.Architecture,
		LogicalCPUCount: snapshot.LogicalCpuCount, MemoryBytes: snapshot.MemoryBytes,
		CPUModel: snapshot.CpuModel, CPUFeatures: append([]string(nil), snapshot.CpuFeatures...),
		CPUBenchmarkScore: snapshot.CpuBenchmarkScore, ComputeWeight: snapshot.ComputeWeight,
		Labels: cloneTextMap(snapshot.Labels), ObservedAt: observedAt.UTC(), ReceivedAt: service.now().UTC(),
		RuntimeFacts: cloneTextMap(snapshot.RuntimeFacts),
	}
	for _, networkInterface := range snapshot.NetworkInterfaces {
		if networkInterface == nil || !validIdentifier(networkInterface.Name, 64) || !validText(networkInterface.MacAddress, 64) ||
			len(networkInterface.Addresses) > 64 || !validTextSlice(networkInterface.Addresses, 256) || networkInterface.Mtu > 1<<20 {
			return ErrInvalidReport
		}
		inventory.NetworkInterfaces = append(inventory.NetworkInterfaces, domain.NetworkInterfaceSnapshot{
			Name: networkInterface.Name, MACAddress: networkInterface.MacAddress,
			Addresses:          append([]string(nil), networkInterface.Addresses...),
			SpeedBitsPerSecond: networkInterface.SpeedBitsPerSecond, MTU: networkInterface.Mtu,
		})
	}
	for _, device := range snapshot.Devices {
		if device == nil || !validIdentifier(device.ResourceName, 128) || !validText(device.Vendor, 256) ||
			!validText(device.Model, 256) || !validText(device.DeviceId, 256) || !validTextMap(device.Attributes, 128, 256, 1024) {
			return ErrInvalidReport
		}
		inventory.Devices = append(inventory.Devices, domain.DeviceSnapshot{
			ResourceName: device.ResourceName, Vendor: device.Vendor, Model: device.Model,
			DeviceID: device.DeviceId, Attributes: cloneTextMap(device.Attributes),
		})
	}
	seenNUMA := make(map[uint32]bool, len(snapshot.NumaNodes))
	for _, node := range snapshot.NumaNodes {
		if node == nil || seenNUMA[node.Id] || !validCPUList(node.Cpus) || node.MemoryBytes > 1<<63-1 {
			return ErrInvalidReport
		}
		seenNUMA[node.Id] = true
		inventory.NUMATopology = append(inventory.NUMATopology, domain.NUMANode{ID: node.Id, CPUs: node.Cpus, MemoryBytes: int64(node.MemoryBytes)})
	}
	return service.repository.PutInventory(ctx, inventory)
}

func validCPUList(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && character != ',' && character != '-' {
			return false
		}
	}
	return true
}

func (service *Service) ReportObservation(ctx context.Context, nodeID, messageID string, observedAt time.Time, report *xirangv1.ResourceObservation) error {
	if !validIdentifier(nodeID, 256) || !validIdentifier(messageID, 256) || report == nil || report.UsedMilliCpu < 0 ||
		len(report.UsedDevices) > 256 || len(report.InterfaceCounters) > 256 || len(report.AcceleratorMetrics) > 256 ||
		!validMetric(report.CpuTemperatureC, 0, 250) || !validMetric(report.GpuTemperatureC, 0, 250) ||
		!validMetric(report.GpuUtilizationPercent, 0, 100) || invalidObservedAt(observedAt, service.now()) {
		return ErrInvalidReport
	}
	if len(report.CpuTemperatureSource) > 128 || (report.CpuTemperatureConfidence != "" && report.CpuTemperatureConfidence != "high" && report.CpuTemperatureConfidence != "medium" && report.CpuTemperatureConfidence != "low") ||
		(report.CpuTemperatureAvailable && report.CpuTemperatureC == 0) {
		return ErrInvalidReport
	}
	for name, quantity := range report.UsedDevices {
		if !validIdentifier(name, 128) || quantity < 0 {
			return ErrInvalidReport
		}
	}
	interfaceCounters := make(map[string]domain.NetworkInterfaceCounters, len(report.InterfaceCounters))
	for _, counter := range report.InterfaceCounters {
		if counter == nil || !validIdentifier(counter.Name, 64) {
			return ErrInvalidReport
		}
		if _, exists := interfaceCounters[counter.Name]; exists {
			return ErrInvalidReport
		}
		interfaceCounters[counter.Name] = domain.NetworkInterfaceCounters{
			ReceivedBytes: counter.ReceivedBytes, TransmittedBytes: counter.TransmittedBytes,
			ReceivedPackets: counter.ReceivedPackets, TransmittedPackets: counter.TransmittedPackets,
			ReceiveErrors: counter.ReceiveErrors, TransmitErrors: counter.TransmitErrors,
			ReceiveDropped: counter.ReceiveDropped, TransmitDropped: counter.TransmitDropped,
			PacketCountersSupported: counter.PacketCountersSupported,
		}
	}
	accelerators := make([]domain.AcceleratorObservation, 0, len(report.AcceleratorMetrics))
	for _, metric := range report.AcceleratorMetrics {
		if metric == nil || !validIdentifier(metric.Id, 256) || !validIdentifier(metric.Vendor, 64) ||
			!validMetric(metric.UtilizationPercent, 0, 100) || !validMetric(metric.TemperatureC, 0, 250) ||
			(metric.MemoryTotalBytes > 0 && metric.MemoryUsedBytes > metric.MemoryTotalBytes) {
			return ErrInvalidReport
		}
		accelerators = append(accelerators, domain.AcceleratorObservation{
			ID: metric.Id, Vendor: metric.Vendor, UtilizationPercent: metric.UtilizationPercent,
			TemperatureC: metric.TemperatureC, MemoryUsedBytes: metric.MemoryUsedBytes,
			MemoryTotalBytes: metric.MemoryTotalBytes, UtilizationAvailable: metric.UtilizationAvailable,
			TemperatureAvailable: metric.TemperatureAvailable,
		})
	}
	return service.repository.AppendObservation(ctx, domain.NodeResourceObservation{
		MessageID: messageID, NodeID: nodeID, UsedMilliCPU: report.UsedMilliCpu,
		UsedMemoryBytes: report.UsedMemoryBytes, ReceivedBytes: report.ReceivedBytes,
		TransmittedBytes: report.TransmittedBytes, UsedDevices: cloneIntMap(report.UsedDevices),
		InterfaceCounters: interfaceCounters,
		CPUTemperatureC:   report.CpuTemperatureC, CPUTemperatureAvailable: report.CpuTemperatureAvailable,
		CPUTemperatureSource: report.CpuTemperatureSource, CPUTemperatureConfidence: report.CpuTemperatureConfidence,
		GPUTemperatureC: report.GpuTemperatureC,
		GPUUtilization:  report.GpuUtilizationPercent, GPUUtilizationAvailable: report.GpuUtilizationAvailable,
		GPUTemperatureAvailable: report.GpuTemperatureAvailable, Accelerators: accelerators,
		ObservedAt: observedAt.UTC(), ReceivedAt: service.now().UTC(),
	})
}

func validMetric(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}

func (service *Service) GetInventory(ctx context.Context, nodeID string) (domain.NodeInventorySnapshot, error) {
	return service.repository.GetInventory(ctx, nodeID)
}

func (service *Service) ListObservations(ctx context.Context, nodeID string, limit int) ([]domain.NodeResourceObservation, error) {
	return service.repository.ListObservations(ctx, nodeID, limit)
}

func validIdentifier(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsRune(value, '\x00')
}

func validText(value string, maximum int) bool {
	return len(value) <= maximum && !strings.ContainsRune(value, '\x00')
}

func validTextSlice(values []string, maximum int) bool {
	for _, value := range values {
		if !validIdentifier(value, maximum) {
			return false
		}
	}
	return true
}

func validTextMap(values map[string]string, maximumItems, maximumKey, maximumValue int) bool {
	if len(values) > maximumItems {
		return false
	}
	for key, value := range values {
		if !validIdentifier(key, maximumKey) || !validText(value, maximumValue) {
			return false
		}
	}
	return true
}

func invalidObservedAt(observedAt, now time.Time) bool {
	return observedAt.IsZero() || observedAt.Before(now.Add(-24*time.Hour)) || observedAt.After(now.Add(5*time.Minute))
}

func cloneTextMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneIntMap(source map[string]int64) map[string]int64 {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]int64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
