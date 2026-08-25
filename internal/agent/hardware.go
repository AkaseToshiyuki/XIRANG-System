package agent

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type cpuCapability struct {
	Model          string
	Features       []string
	BenchmarkScore float64
}

type nvidiaGPU struct {
	PCIAddress           string
	Name                 string
	UUID                 string
	ComputeCapability    string
	SMCount              int
	CoresPerSM           int
	CUDACores            int
	Utilization          float64
	Temperature          float64
	UtilizationAvailable bool
	TemperatureAvailable bool
	MemoryUsedBytes      uint64
	MemoryTotalBytes     uint64
}

var (
	cpuCapabilityOnce sync.Once
	cachedCPU         cpuCapability
)

func detectCPUCapability() cpuCapability {
	cpuCapabilityOnce.Do(func() {
		cachedCPU.Model, cachedCPU.Features = parseCPUInfo("/proc/cpuinfo")
		if cachedCPU.Model == "" {
			for _, path := range []string{"/sys/firmware/devicetree/base/model", "/proc/device-tree/model"} {
				if model := strings.Trim(strings.TrimSpace(readText(path)), "\x00"); model != "" {
					cachedCPU.Model = model
					break
				}
			}
		}
		if cachedCPU.Model == "" {
			cachedCPU.Model = cpuModelFromLSCPU()
		}
		if cachedCPU.Model == "" {
			cachedCPU.Model = strings.ToUpper(runtime.GOARCH) + " processor"
		}
		cachedCPU.BenchmarkScore = benchmarkCPU(160 * time.Millisecond)
	})
	return cachedCPU
}

func cpuModelFromLSCPU() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "lscpu")
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if err != nil {
		return ""
	}
	seen := map[string]bool{}
	var models []string
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != "Model name" {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			models = append(models, value)
		}
	}
	return strings.Join(models, " + ")
}

func parseCPUInfo(path string) (string, []string) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil
	}
	defer file.Close()
	features := map[string]bool{}
	model := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(strings.ToLower(key)), strings.TrimSpace(value)
		if (key == "model name" || key == "hardware") && value != "" {
			model = value
		} else if model == "" && key == "processor" && strings.IndexFunc(value, func(character rune) bool {
			return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		}) >= 0 {
			model = value
		}
		if key == "flags" || key == "features" {
			for _, feature := range strings.Fields(strings.ToLower(value)) {
				if len(feature) <= 64 {
					features[feature] = true
				}
			}
		}
	}
	result := make([]string, 0, len(features))
	for feature := range features {
		result = append(result, feature)
	}
	sort.Strings(result)
	return model, result
}

// benchmarkCPU is a bounded integer-throughput probe. It intentionally runs
// for less than one heartbeat and allocates no large working set, making it
// suitable for first enrollment on heterogeneous bare-metal nodes.
func benchmarkCPU(duration time.Duration) float64 {
	if duration <= 0 {
		return 0
	}
	workers := runtime.NumCPU()
	if workers > 256 {
		workers = 256
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var operations atomic.Uint64
	var group sync.WaitGroup
	group.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(seed uint64) {
			defer group.Done()
			value := seed + 0x9e3779b97f4a7c15
			var local uint64
			for {
				for range 4096 {
					value ^= value << 13
					value ^= value >> 7
					value ^= value << 17
					local++
				}
				select {
				case <-ctx.Done():
					operations.Add(local)
					runtime.KeepAlive(value)
					return
				default:
				}
			}
		}(uint64(worker + 1))
	}
	group.Wait()
	elapsed := time.Since(started).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return math.Round((float64(operations.Load())/elapsed/1e9)*100) / 100
}

func detectNVIDIAGPUs(includeMetrics bool) []nvidiaGPU {
	// multiprocessor_count is not exposed by a number of otherwise supported
	// NVIDIA driver branches. Query only portable nvidia-smi fields and derive
	// SM count from the known model profile when the driver omits it.
	fields := "pci.bus_id,name,uuid,compute_cap"
	if includeMetrics {
		fields += ",utilization.gpu,temperature.gpu,memory.used,memory.total"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu="+fields, "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNVIDIASMI(output, includeMetrics)
}

func parseNVIDIASMI(output []byte, includeMetrics bool) []nvidiaGPU {
	var result []nvidiaGPU
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.Split(line, ",")
		expected := 4
		if includeMetrics {
			expected = 8
		}
		if len(parts) != expected {
			continue
		}
		for index := range parts {
			parts[index] = strings.TrimSpace(parts[index])
		}
		gpu := nvidiaGPU{
			PCIAddress: normalizePCIAddress(parts[0]), Name: parts[1], UUID: parts[2],
			ComputeCapability: parts[3],
		}
		_, gpu.SMCount = knownNVIDIAProfile(gpu.Name, "")
		gpu.CoresPerSM = cudaCoresPerSM(gpu.ComputeCapability)
		gpu.CUDACores = gpu.SMCount * gpu.CoresPerSM
		if includeMetrics {
			var parseErr error
			gpu.Utilization, parseErr = strconv.ParseFloat(parts[4], 64)
			gpu.UtilizationAvailable = parseErr == nil
			gpu.Temperature, parseErr = strconv.ParseFloat(parts[5], 64)
			gpu.TemperatureAvailable = parseErr == nil
			usedMiB, _ := strconv.ParseUint(parts[6], 10, 64)
			totalMiB, _ := strconv.ParseUint(parts[7], 10, 64)
			gpu.MemoryUsedBytes, gpu.MemoryTotalBytes = usedMiB<<20, totalMiB<<20
		}
		result = append(result, gpu)
	}
	return result
}

func normalizePCIAddress(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 16 && strings.Count(value, ":") == 2 {
		return "0000:" + value[9:]
	}
	if len(value) == 12 && strings.Count(value, ":") == 2 {
		return value
	}
	if len(value) == 8 && strings.Count(value, ":") == 1 {
		return "0000:" + value
	}
	return value
}

// Values are a coarse CUDA-core proxy for compute capabilities represented by
// the supported model profiles. They are used only for relative scheduling
// weight, not as a FLOP/s estimate. Unknown profiles contribute no GPU term.
func cudaCoresPerSM(capability string) int {
	table := map[string]int{
		"3.0": 192, "3.2": 192, "3.5": 192, "3.7": 192,
		"5.0": 128, "5.2": 128, "5.3": 128,
		"6.0": 64, "6.1": 128, "6.2": 128,
		"7.0": 64, "7.2": 64, "7.5": 64,
		"8.0": 64, "8.6": 128, "8.7": 128, "8.9": 128,
		"9.0": 128, "10.0": 128, "12.0": 128,
	}
	return table[capability]
}

func knownNVIDIAProfile(model, deviceID string) (string, int) {
	type profile struct {
		capability string
		smCount    int
	}
	byDevice := map[string]profile{
		"0x1bb3": {"6.1", 20},  // Tesla P4
		"0x2206": {"8.6", 68},  // GeForce RTX 3080
		"0x2204": {"8.6", 82},  // GeForce RTX 3090
		"0x2684": {"8.9", 128}, // GeForce RTX 4090
		"0x20b0": {"8.0", 108}, // A100
		"0x2330": {"9.0", 114}, // H100 PCIe
		"0x26b5": {"8.9", 142}, // L40S
	}
	if candidate, ok := byDevice[strings.ToLower(strings.TrimSpace(deviceID))]; ok {
		return candidate.capability, candidate.smCount
	}
	normalized := strings.ToLower(model)
	for name, candidate := range map[string]profile{
		"tesla p4": {"6.1", 20},
		"rtx 3080": {"8.6", 68}, "rtx 3090": {"8.6", 82}, "rtx 4090": {"8.9", 128},
		"a100": {"8.0", 108}, "h100": {"9.0", 114}, "l40s": {"8.9", 142},
	} {
		if strings.Contains(normalized, name) {
			return candidate.capability, candidate.smCount
		}
	}
	return "", 0
}

func computeWeight(cpuScore float64, devices []DeviceInventory) float64 {
	weight := cpuScore
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = float64(runtime.NumCPU())
	}
	for _, device := range devices {
		if value, err := strconv.ParseFloat(device.Attributes["smComputeScore"], 64); err == nil && value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			weight += value / 1000
		}
	}
	return math.Round(weight*100) / 100
}

func fallbackGPUModel(vendor, deviceID string) string {
	name := map[string]string{"nvidia": "NVIDIA", "amd": "AMD", "intel": "Intel"}[vendor]
	if name == "" {
		name = "GPU"
	}
	return fmt.Sprintf("%s GPU %s", name, strings.TrimPrefix(deviceID, "0x"))
}

func pciDisplayModel(address, fallback string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "lspci", "-s", address, "-mm").Output()
	if err != nil {
		return fallback
	}
	var quoted []string
	text := string(output)
	for {
		start := strings.IndexByte(text, '"')
		if start < 0 {
			break
		}
		text = text[start+1:]
		end := strings.IndexByte(text, '"')
		if end < 0 {
			break
		}
		quoted = append(quoted, text[:end])
		text = text[end+1:]
	}
	if len(quoted) >= 3 {
		return strings.TrimSpace(quoted[1] + " " + quoted[2])
	}
	return fallback
}
