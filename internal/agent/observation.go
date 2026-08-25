package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type ResourceObservation struct {
	UsedMilliCPU             int64                      `json:"usedMilliCpu"`
	UsedMemoryBytes          uint64                     `json:"usedMemoryBytes"`
	ReceivedBytes            uint64                     `json:"receivedBytes"`
	TransmittedBytes         uint64                     `json:"transmittedBytes"`
	InterfaceCounters        []NetworkInterfaceCounters `json:"interfaceCounters"`
	CPUTemperatureC          float64                    `json:"cpuTemperatureC,omitempty"`
	CPUTemperatureAvailable  bool                       `json:"cpuTemperatureAvailable"`
	CPUTemperatureSource     string                     `json:"cpuTemperatureSource,omitempty"`
	CPUTemperatureConfidence string                     `json:"cpuTemperatureConfidence,omitempty"`
	GPUTemperatureC          float64                    `json:"gpuTemperatureC,omitempty"`
	GPUUtilization           float64                    `json:"gpuUtilizationPercent,omitempty"`
	GPUUtilizationAvailable  bool                       `json:"gpuUtilizationAvailable"`
	GPUTemperatureAvailable  bool                       `json:"gpuTemperatureAvailable"`
	Accelerators             []AcceleratorObservation   `json:"accelerators,omitempty"`
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
	Name                    string `json:"name"`
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

type cpuCounters struct {
	total uint64
	idle  uint64
}

type ResourceSampler struct {
	statPath             string
	meminfoPath          string
	netdevPath           string
	cpuCount             int64
	previousCPU          cpuCounters
	hwmonRoot            string
	drmRoot              string
	temperatureProviders []CPUTemperatureProvider
}

func NewResourceSampler() (*ResourceSampler, error) {
	sampler, err := newResourceSampler("/proc/stat", "/proc/meminfo", "/proc/net/dev", int64(runtime.NumCPU()))
	if err == nil {
		sampler.temperatureProviders = configuredTemperatureProviders()
	}
	return sampler, err
}

func newResourceSampler(statPath, meminfoPath, netdevPath string, cpuCount int64) (*ResourceSampler, error) {
	if cpuCount <= 0 {
		return nil, errors.New("logical CPU count must be positive")
	}
	previous, err := readCPUCounters(statPath)
	if err != nil {
		return nil, err
	}
	return &ResourceSampler{
		statPath: statPath, meminfoPath: meminfoPath, netdevPath: netdevPath,
		cpuCount: cpuCount, previousCPU: previous, hwmonRoot: "/sys/class/hwmon", drmRoot: "/sys/class/drm",
	}, nil
}

func (sampler *ResourceSampler) Sample() (ResourceObservation, error) {
	current, err := readCPUCounters(sampler.statPath)
	if err != nil {
		return ResourceObservation{}, err
	}
	usedMilliCPU, err := cpuUsageMilli(sampler.previousCPU, current, sampler.cpuCount)
	if err != nil {
		return ResourceObservation{}, err
	}
	sampler.previousCPU = current
	usedMemory, err := readUsedMemory(sampler.meminfoPath)
	if err != nil {
		return ResourceObservation{}, err
	}
	received, transmitted, interfaces, err := readNetworkCounters(sampler.netdevPath)
	if err != nil {
		return ResourceObservation{}, err
	}
	cpuTemperature, gpuTemperature, gpuUtilization, gpuUtilizationAvailable, gpuTemperatureAvailable, accelerators := collectHardwareMetrics(sampler.hwmonRoot, sampler.drmRoot)
	cpuReading := CPUTemperatureReading{Celsius: cpuTemperature, Source: "hwmon/thermal-zone", Confidence: "high"}
	if cpuTemperature == 0 {
		for _, provider := range sampler.temperatureProviders {
			reading, providerErr := provider.ReadCPUTemperature(context.Background())
			if providerErr == nil && validTemperature(reading.Celsius) {
				cpuReading = reading
				break
			}
		}
	}
	return ResourceObservation{
		UsedMilliCPU: usedMilliCPU, UsedMemoryBytes: usedMemory,
		ReceivedBytes: received, TransmittedBytes: transmitted,
		InterfaceCounters: interfaces,
		CPUTemperatureC:   cpuReading.Celsius, CPUTemperatureAvailable: validTemperature(cpuReading.Celsius),
		CPUTemperatureSource: cpuReading.Source, CPUTemperatureConfidence: cpuReading.Confidence,
		GPUTemperatureC: gpuTemperature,
		GPUUtilization:  gpuUtilization, GPUUtilizationAvailable: gpuUtilizationAvailable,
		GPUTemperatureAvailable: gpuTemperatureAvailable, Accelerators: accelerators,
	}, nil
}

func readCPUCounters(path string) (cpuCounters, error) {
	file, err := os.Open(path)
	if err != nil {
		return cpuCounters{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return cpuCounters{}, errors.New("/proc/stat does not contain aggregate CPU counters")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, errors.New("/proc/stat aggregate CPU counters are malformed")
	}
	values := make([]uint64, 0, 8)
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuCounters{}, fmt.Errorf("parse /proc/stat CPU counter: %w", err)
		}
		values = append(values, value)
		if len(values) == 8 {
			break
		}
	}
	if len(values) < 4 {
		return cpuCounters{}, errors.New("/proc/stat aggregate CPU counters are incomplete")
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuCounters{total: total, idle: idle}, nil
}

func cpuUsageMilli(previous, current cpuCounters, cpuCount int64) (int64, error) {
	if current.total <= previous.total || current.idle < previous.idle {
		return 0, errors.New("aggregate CPU counters did not increase")
	}
	totalDelta := current.total - previous.total
	idleDelta := current.idle - previous.idle
	if idleDelta > totalDelta {
		return 0, errors.New("aggregate idle CPU counter exceeds total counter")
	}
	used := int64((totalDelta - idleDelta) * uint64(cpuCount*1000) / totalDelta)
	maximum := cpuCount * 1000
	if used > maximum {
		used = maximum
	}
	return used, nil
}

func readUsedMemory(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var total, available uint64
	var foundAvailable bool
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value * 1024
		case "MemAvailable:":
			available = value * 1024
			foundAvailable = true
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if total == 0 || !foundAvailable || available > total {
		return 0, errors.New("/proc/meminfo memory counters are incomplete")
	}
	return total - available, nil
}

func readNetworkCounters(path string) (uint64, uint64, []NetworkInterfaceCounters, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, nil, err
	}
	defer file.Close()
	var received, transmitted uint64
	var interfaces []NetworkInterfaceCounters
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, counters, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return 0, 0, nil, errors.New("/proc/net/dev interface name is empty")
		}
		fields := strings.Fields(counters)
		if len(fields) < 12 {
			return 0, 0, nil, errors.New("/proc/net/dev interface counters are malformed")
		}
		indexes := [...]int{0, 8, 1, 9, 2, 10, 3, 11}
		values := [len(indexes)]uint64{}
		for index, fieldIndex := range indexes {
			value, err := strconv.ParseUint(fields[fieldIndex], 10, 64)
			if err != nil {
				return 0, 0, nil, err
			}
			values[index] = value
		}
		received += values[0]
		transmitted += values[1]
		interfaces = append(interfaces, NetworkInterfaceCounters{
			Name: name, ReceivedBytes: values[0], TransmittedBytes: values[1],
			ReceivedPackets: values[2], TransmittedPackets: values[3],
			ReceiveErrors: values[4], TransmitErrors: values[5],
			ReceiveDropped: values[6], TransmitDropped: values[7],
			PacketCountersSupported: true,
		})
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, nil, err
	}
	sort.Slice(interfaces, func(left, right int) bool { return interfaces[left].Name < interfaces[right].Name })
	return received, transmitted, interfaces, nil
}
