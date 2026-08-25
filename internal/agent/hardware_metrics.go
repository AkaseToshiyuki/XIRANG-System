package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func collectHardwareMetrics(hwmonRoot, drmRoot string) (float64, float64, float64, bool, bool, []AcceleratorObservation) {
	cpuTemperature := collectCPUTemperature(hwmonRoot)
	accelerators := make([]AcceleratorObservation, 0)
	seenPCI := map[string]bool{}
	for _, gpu := range detectNVIDIAGPUs(true) {
		accelerators = append(accelerators, AcceleratorObservation{
			ID: gpu.UUID, Vendor: "nvidia", UtilizationPercent: boundedPercent(gpu.Utilization),
			TemperatureC: gpu.Temperature, MemoryUsedBytes: gpu.MemoryUsedBytes, MemoryTotalBytes: gpu.MemoryTotalBytes,
			UtilizationAvailable: gpu.UtilizationAvailable, TemperatureAvailable: gpu.TemperatureAvailable,
		})
		seenPCI[gpu.PCIAddress] = true
	}
	cardPaths, _ := filepath.Glob(filepath.Join(drmRoot, "card*", "device"))
	for _, devicePath := range cardPaths {
		resolved, err := filepath.EvalSymlinks(devicePath)
		if err != nil {
			continue
		}
		pciAddress := normalizePCIAddress(filepath.Base(resolved))
		if seenPCI[pciAddress] {
			continue
		}
		vendorID := strings.TrimSpace(readText(filepath.Join(devicePath, "vendor")))
		vendor := map[string]string{"0x1002": "amd", "0x10de": "nvidia", "0x8086": "intel"}[vendorID]
		if vendor == "" {
			continue
		}
		utilizationText := strings.TrimSpace(readText(filepath.Join(devicePath, "gpu_busy_percent")))
		utilization, utilizationErr := strconv.ParseFloat(utilizationText, 64)
		temperature := collectDeviceTemperature(filepath.Join(devicePath, "hwmon"))
		accelerators = append(accelerators, AcceleratorObservation{
			ID: pciAddress, Vendor: vendor, UtilizationPercent: boundedPercent(utilization), TemperatureC: temperature,
			UtilizationAvailable: utilizationErr == nil && utilizationText != "", TemperatureAvailable: temperature > 0,
		})
	}
	var maximumTemperature, utilizationSum float64
	var utilizationCount int
	var temperatureAvailable bool
	for _, metric := range accelerators {
		if metric.TemperatureAvailable && metric.TemperatureC > maximumTemperature {
			maximumTemperature = metric.TemperatureC
			temperatureAvailable = true
		}
		if metric.UtilizationAvailable {
			utilizationSum += metric.UtilizationPercent
			utilizationCount++
		}
	}
	averageUtilization := float64(0)
	if utilizationCount > 0 {
		averageUtilization = utilizationSum / float64(utilizationCount)
	}
	return cpuTemperature, maximumTemperature, averageUtilization, utilizationCount > 0, temperatureAvailable, accelerators
}

func collectCPUTemperature(root string) float64 {
	entries, err := os.ReadDir(root)
	if err != nil {
		if root == "/sys/class/hwmon" {
			return collectThermalZoneTemperature("/sys/class/thermal")
		}
		return 0
	}
	var maximum float64
	for _, entry := range entries {
		deviceRoot := filepath.Join(root, entry.Name())
		name := strings.ToLower(strings.TrimSpace(readText(filepath.Join(deviceRoot, "name"))))
		value := float64(0)
		if strings.Contains(name, "coretemp") || strings.Contains(name, "k10temp") ||
			strings.Contains(name, "cpu") || strings.Contains(name, "soc") || strings.Contains(name, "acpitz") {
			value = collectDeviceTemperature(deviceRoot)
		} else {
			// Some ARM and server hwmon drivers use a generic device name but
			// identify CPU sensors through temp*_label.
			value = collectLabeledCPUTemperature(deviceRoot)
		}
		if value > maximum {
			maximum = value
		}
	}
	if maximum == 0 && root == "/sys/class/hwmon" {
		return collectThermalZoneTemperature("/sys/class/thermal")
	}
	return maximum
}

func collectThermalZoneTemperature(root string) float64 {
	zones, _ := filepath.Glob(filepath.Join(root, "thermal_zone*"))
	var maximum float64
	for _, zone := range zones {
		kind := strings.ToLower(strings.TrimSpace(readText(filepath.Join(zone, "type"))))
		if !strings.Contains(kind, "cpu") && !strings.Contains(kind, "soc") && !strings.Contains(kind, "package") &&
			!strings.Contains(kind, "x86_pkg") && !strings.Contains(kind, "acpitz") {
			continue
		}
		raw, err := strconv.ParseFloat(strings.TrimSpace(readText(filepath.Join(zone, "temp"))), 64)
		if err != nil {
			continue
		}
		if raw > 1000 {
			raw /= 1000
		}
		if raw > maximum && raw < 250 {
			maximum = raw
		}
	}
	return maximum
}

func collectLabeledCPUTemperature(root string) float64 {
	labels, _ := filepath.Glob(filepath.Join(root, "temp*_label"))
	var maximum float64
	for _, labelPath := range labels {
		label := strings.ToLower(strings.TrimSpace(readText(labelPath)))
		if strings.Contains(label, "gpu") || strings.Contains(label, "nvme") ||
			strings.Contains(label, "wifi") || strings.Contains(label, "ambient") {
			continue
		}
		if !strings.Contains(label, "cpu") && !strings.Contains(label, "soc") &&
			!strings.Contains(label, "package") && !strings.Contains(label, "tdie") &&
			!strings.Contains(label, "tctl") && !strings.Contains(label, "core") {
			continue
		}
		inputPath := strings.TrimSuffix(labelPath, "_label") + "_input"
		if value := readTemperature(inputPath); value > maximum {
			maximum = value
		}
	}
	return maximum
}

func collectDeviceTemperature(root string) float64 {
	paths, _ := filepath.Glob(filepath.Join(root, "temp*_input"))
	var maximum float64
	for _, path := range paths {
		if value := readTemperature(path); value > maximum {
			maximum = value
		}
	}
	return maximum
}

func readTemperature(path string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(readText(path)), 64)
	if err != nil {
		return 0
	}
	if value > 1000 {
		value /= 1000
	}
	if value <= 0 || value >= 250 {
		return 0
	}
	return value
}

func boundedPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
