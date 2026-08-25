package domain

import "time"

type NetworkInterfaceSnapshot struct {
	Name               string   `json:"name"`
	MACAddress         string   `json:"macAddress,omitempty"`
	Addresses          []string `json:"addresses,omitempty"`
	SpeedBitsPerSecond uint64   `json:"speedBitsPerSecond,omitempty"`
	MTU                uint32   `json:"mtu,omitempty"`
}

type DeviceSnapshot struct {
	ResourceName string            `json:"resourceName"`
	Vendor       string            `json:"vendor,omitempty"`
	Model        string            `json:"model,omitempty"`
	DeviceID     string            `json:"deviceId,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

type NodeInventorySnapshot struct {
	NodeID            string                     `json:"nodeId"`
	Hostname          string                     `json:"hostname"`
	Distribution      string                     `json:"distribution,omitempty"`
	KernelVersion     string                     `json:"kernelVersion,omitempty"`
	Architecture      string                     `json:"architecture"`
	LogicalCPUCount   uint32                     `json:"logicalCpuCount"`
	CPUModel          string                     `json:"cpuModel,omitempty"`
	CPUFeatures       []string                   `json:"cpuFeatures,omitempty"`
	CPUBenchmarkScore float64                    `json:"cpuBenchmarkScore,omitempty"`
	ComputeWeight     float64                    `json:"computeWeight,omitempty"`
	MemoryBytes       uint64                     `json:"memoryBytes"`
	NetworkInterfaces []NetworkInterfaceSnapshot `json:"networkInterfaces,omitempty"`
	Devices           []DeviceSnapshot           `json:"devices,omitempty"`
	NUMATopology      []NUMANode                 `json:"numaTopology,omitempty"`
	Labels            map[string]string          `json:"labels,omitempty"`
	ObservedAt        time.Time                  `json:"observedAt"`
	ReceivedAt        time.Time                  `json:"receivedAt"`
}

type NodeResourceObservation struct {
	MessageID                string                              `json:"messageId"`
	NodeID                   string                              `json:"nodeId"`
	UsedMilliCPU             int64                               `json:"usedMilliCpu"`
	UsedMemoryBytes          uint64                              `json:"usedMemoryBytes"`
	ReceivedBytes            uint64                              `json:"receivedBytes"`
	TransmittedBytes         uint64                              `json:"transmittedBytes"`
	UsedDevices              map[string]int64                    `json:"usedDevices,omitempty"`
	InterfaceCounters        map[string]NetworkInterfaceCounters `json:"interfaceCounters,omitempty"`
	CPUTemperatureC          float64                             `json:"cpuTemperatureC,omitempty"`
	CPUTemperatureAvailable  bool                                `json:"cpuTemperatureAvailable"`
	CPUTemperatureSource     string                              `json:"cpuTemperatureSource,omitempty"`
	CPUTemperatureConfidence string                              `json:"cpuTemperatureConfidence,omitempty"`
	GPUTemperatureC          float64                             `json:"gpuTemperatureC,omitempty"`
	GPUUtilization           float64                             `json:"gpuUtilizationPercent,omitempty"`
	GPUUtilizationAvailable  bool                                `json:"gpuUtilizationAvailable"`
	GPUTemperatureAvailable  bool                                `json:"gpuTemperatureAvailable"`
	Accelerators             []AcceleratorObservation            `json:"accelerators,omitempty"`
	ObservedAt               time.Time                           `json:"observedAt"`
	ReceivedAt               time.Time                           `json:"receivedAt"`
}

type AcceleratorObservation struct {
	ID                   string  `json:"id"`
	Vendor               string  `json:"vendor"`
	UtilizationPercent   float64 `json:"utilizationPercent,omitempty"`
	TemperatureC         float64 `json:"temperatureC,omitempty"`
	MemoryUsedBytes      uint64  `json:"memoryUsedBytes,omitempty"`
	MemoryTotalBytes     uint64  `json:"memoryTotalBytes,omitempty"`
	UtilizationAvailable bool    `json:"utilizationAvailable"`
	TemperatureAvailable bool    `json:"temperatureAvailable"`
}

type NetworkInterfaceCounters struct {
	ReceivedBytes           uint64 `json:"receivedBytes"`
	TransmittedBytes        uint64 `json:"transmittedBytes"`
	ReceivedPackets         uint64 `json:"receivedPackets"`
	TransmittedPackets      uint64 `json:"transmittedPackets"`
	ReceiveErrors           uint64 `json:"receiveErrors"`
	TransmitErrors          uint64 `json:"transmitErrors"`
	ReceiveDropped          uint64 `json:"receiveDropped"`
	TransmitDropped         uint64 `json:"transmitDropped"`
	PacketCountersSupported bool   `json:"packetCountersSupported"`
}
