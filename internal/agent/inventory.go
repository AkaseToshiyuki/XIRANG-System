// Package agent contains node-local capabilities that do not depend on the
// control-plane transport.
package agent

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/xirang-grid/xirang/pkg/version"
)

type Inventory struct {
	Hostname              string                      `json:"hostname"`
	OS                    string                      `json:"os"`
	Architecture          string                      `json:"architecture"`
	Distribution          string                      `json:"distribution,omitempty"`
	KernelVersion         string                      `json:"kernelVersion,omitempty"`
	RuntimeFacts          map[string]string           `json:"runtimeFacts,omitempty"`
	AgentVersion          string                      `json:"agentVersion"`
	CPUCount              int                         `json:"cpuCount"`
	CPUModel              string                      `json:"cpuModel,omitempty"`
	CPUFeatures           []string                    `json:"cpuFeatures,omitempty"`
	CPUBenchmarkScore     float64                     `json:"cpuBenchmarkScore,omitempty"`
	ComputeWeight         float64                     `json:"computeWeight,omitempty"`
	MemoryBytes           int64                       `json:"memoryBytes,omitempty"`
	EphemeralStorageBytes int64                       `json:"ephemeralStorageBytes,omitempty"`
	Labels                map[string]string           `json:"labels,omitempty"`
	NetworkInterfaces     []NetworkInterfaceInventory `json:"networkInterfaces,omitempty"`
	Devices               []DeviceInventory           `json:"devices,omitempty"`
	NUMATopology          []NUMANodeInventory         `json:"numaTopology,omitempty"`
}

type NUMANodeInventory struct {
	ID          uint32 `json:"id"`
	CPUs        string `json:"cpus"`
	MemoryBytes int64  `json:"memoryBytes"`
}

type DeviceInventory struct {
	ResourceName string            `json:"resourceName"`
	Vendor       string            `json:"vendor,omitempty"`
	Model        string            `json:"model,omitempty"`
	DeviceID     string            `json:"deviceId,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

type NetworkInterfaceInventory struct {
	Name               string   `json:"name"`
	MACAddress         string   `json:"macAddress,omitempty"`
	Addresses          []string `json:"addresses,omitempty"`
	SpeedBitsPerSecond uint64   `json:"speedBitsPerSecond,omitempty"`
	MTU                int      `json:"mtu,omitempty"`
}

// CollectInventory returns a conservative inventory that works without root.
// Device plugins can extend the snapshot with accelerators and other resources.
func CollectInventory() (Inventory, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Inventory{}, err
	}
	interfaces, err := collectNetworkInterfaces("/sys/class/net")
	if err != nil {
		return Inventory{}, err
	}
	cpu := detectCPUCapability()
	devices := collectAccelerators("/sys/class/drm", "/proc/driver/nvidia/gpus")
	kernelVersion := strings.TrimSpace(readText("/proc/sys/kernel/osrelease"))
	return Inventory{
		Hostname:              hostname,
		OS:                    runtime.GOOS,
		Architecture:          runtime.GOARCH,
		Distribution:          distributionName("/etc/os-release"),
		KernelVersion:         kernelVersion,
		RuntimeFacts:          collectRuntimeFacts(kernelVersion),
		AgentVersion:          version.Version,
		CPUCount:              runtime.NumCPU(),
		CPUModel:              cpu.Model,
		CPUFeatures:           cpu.Features,
		CPUBenchmarkScore:     cpu.BenchmarkScore,
		ComputeWeight:         computeWeight(cpu.BenchmarkScore, devices),
		MemoryBytes:           linuxMemoryBytes("/proc/meminfo"),
		EphemeralStorageBytes: availableFilesystemBytes(),
		Labels:                map[string]string{"xirang.io/arch": runtime.GOARCH},
		NetworkInterfaces:     interfaces,
		Devices:               devices,
		NUMATopology:          collectNUMATopology("/sys/devices/system/node"),
	}, nil
}

func collectRuntimeFacts(kernelVersion string) map[string]string {
	facts := map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH}
	if kernelVersion != "" {
		facts["kernel.version"] = kernelVersion
	}
	if output, err := exec.Command("getconf", "GNU_LIBC_VERSION").Output(); err == nil {
		parts := strings.Fields(strings.TrimSpace(string(output)))
		if len(parts) >= 2 {
			facts["libc.family"], facts["libc.version"] = strings.ToLower(parts[0]), parts[1]
		}
	}
	return facts
}

func availableFilesystemBytes() int64 {
	root := "/"
	if info, err := os.Stat("/var/lib/xirang-tasks"); err == nil && info.IsDir() {
		root = "/var/lib/xirang-tasks"
	}
	var statistics syscall.Statfs_t
	if err := syscall.Statfs(root, &statistics); err != nil || statistics.Bavail == 0 {
		return 0
	}
	maximum := uint64(^uint64(0) >> 1)
	blockSize := uint64(statistics.Bsize)
	if blockSize == 0 || uint64(statistics.Bavail) > maximum/blockSize {
		return int64(maximum)
	}
	return int64(uint64(statistics.Bavail) * blockSize)
}

func collectAccelerators(drmRoot, nvidiaRoot string) []DeviceInventory {
	devices := make([]DeviceInventory, 0)
	nvidiaDetails := map[string]nvidiaGPU{}
	for _, gpu := range detectNVIDIAGPUs(false) {
		nvidiaDetails[gpu.PCIAddress] = gpu
	}
	if entries, err := os.ReadDir(nvidiaRoot); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				attributes := map[string]string{"pciAddress": entry.Name()}
				informationPath := filepath.Join(nvidiaRoot, entry.Name(), "information")
				information := nvidiaDeviceInformation(informationPath)
				if minor := information["Device Minor"]; minor != "" {
					attributes["devicePath"] = "/dev/nvidia" + minor
				}
				if numa := normalizedNUMANode(readText(filepath.Join("/sys/bus/pci/devices", entry.Name(), "numa_node"))); numa != "" {
					attributes["numaNode"] = numa
				}
				model := ""
				if gpu, ok := nvidiaDetails[normalizePCIAddress(entry.Name())]; ok {
					model = gpu.Name
					attributes["uuid"] = gpu.UUID
					attributes["computeCapability"] = gpu.ComputeCapability
					attributes["smCount"] = strconv.Itoa(gpu.SMCount)
					attributes["coresPerSM"] = strconv.Itoa(gpu.CoresPerSM)
					attributes["smComputeScore"] = strconv.Itoa(gpu.CUDACores)
				}
				if model == "" {
					model = information["Model"]
					if uuid := information["GPU UUID"]; uuid != "" {
						attributes["uuid"] = uuid
					}
					capability, smCount := knownNVIDIAProfile(model, strings.TrimSpace(readText(filepath.Join("/sys/bus/pci/devices", entry.Name(), "device"))))
					if capability != "" {
						coresPerSM := cudaCoresPerSM(capability)
						attributes["computeCapability"] = capability
						attributes["smCount"] = strconv.Itoa(smCount)
						attributes["coresPerSM"] = strconv.Itoa(coresPerSM)
						attributes["smComputeScore"] = strconv.Itoa(smCount * coresPerSM)
					}
				}
				if model == "" {
					model = fallbackGPUModel("nvidia", strings.TrimSpace(readText(filepath.Join("/sys/bus/pci/devices", entry.Name(), "device"))))
				}
				nvidiaDetails[normalizePCIAddress(entry.Name())] = nvidiaGPU{Name: model}
				devices = append(devices, DeviceInventory{ResourceName: "nvidia.com/gpu", Vendor: "nvidia", Model: model, DeviceID: entry.Name(), Attributes: attributes})
			}
		}
	}
	if entries, err := os.ReadDir(drmRoot); err == nil {
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "renderD") {
				continue
			}
			vendorID := strings.TrimSpace(readText(filepath.Join(drmRoot, entry.Name(), "device", "vendor")))
			vendor := map[string]string{"0x1002": "amd", "0x10de": "nvidia", "0x8086": "intel"}[vendorID]
			if vendor == "" {
				continue
			}
			deviceID := strings.TrimSpace(readText(filepath.Join(drmRoot, entry.Name(), "device", "device")))
			pciAddress := filepath.Base(filepath.Clean(filepath.Join(drmRoot, entry.Name(), "device")))
			if resolved, err := filepath.EvalSymlinks(filepath.Join(drmRoot, entry.Name(), "device")); err == nil {
				pciAddress = filepath.Base(resolved)
			}
			attributes := map[string]string{"vendorId": vendorID, "pciDeviceId": deviceID, "pciAddress": pciAddress, "devicePath": filepath.Join("/dev/dri", entry.Name())}
			if numa := normalizedNUMANode(readText(filepath.Join(drmRoot, entry.Name(), "device", "numa_node"))); numa != "" {
				attributes["numaNode"] = numa
			}
			if vendor == "nvidia" {
				if _, exists := nvidiaDetails[normalizePCIAddress(pciAddress)]; exists {
					continue
				}
			}
			devices = append(devices, DeviceInventory{
				ResourceName: "xirang.io/drm-render", Vendor: vendor, Model: pciDisplayModel(pciAddress, fallbackGPUModel(vendor, deviceID)), DeviceID: entry.Name(),
				Attributes: attributes,
			})
		}
	}
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].ResourceName+"\x00"+devices[i].DeviceID < devices[j].ResourceName+"\x00"+devices[j].DeviceID
	})
	return devices
}

func collectNUMATopology(root string) []NUMANodeInventory {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	nodes := make([]NUMANodeInventory, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "node") {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(entry.Name(), "node"), 10, 32)
		if err != nil {
			continue
		}
		nodes = append(nodes, NUMANodeInventory{
			ID: uint32(id), CPUs: strings.TrimSpace(readText(filepath.Join(root, entry.Name(), "cpulist"))),
			MemoryBytes: numaMemoryBytes(filepath.Join(root, entry.Name(), "meminfo")),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes
}

func numaMemoryBytes(path string) int64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 4 && fields[len(fields)-3] == "MemTotal:" && fields[len(fields)-1] == "kB" {
			value, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
			if err == nil && value <= (1<<63-1)/1024 {
				return value * 1024
			}
		}
	}
	return 0
}

func nvidiaDeviceMinor(path string) string {
	return nvidiaDeviceInformation(path)["Device Minor"]
}

func nvidiaDeviceInformation(path string) map[string]string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	result := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if found {
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if key == "Device Minor" {
				if _, err := strconv.ParseUint(value, 10, 32); err != nil {
					continue
				}
			}
			result[key] = value
		}
	}
	return result
}

func normalizedNUMANode(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 0 {
		return ""
	}
	return strconv.FormatInt(parsed, 10)
}

func collectNetworkInterfaces(sysClassNet string) ([]NetworkInterfaceInventory, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]NetworkInterfaceInventory, 0, len(interfaces))
	for _, networkInterface := range interfaces {
		addresses, err := networkInterface.Addrs()
		if err != nil {
			return nil, err
		}
		encodedAddresses := make([]string, 0, len(addresses))
		for _, address := range addresses {
			encodedAddresses = append(encodedAddresses, address.String())
		}
		sort.Strings(encodedAddresses)
		result = append(result, NetworkInterfaceInventory{
			Name: networkInterface.Name, MACAddress: networkInterface.HardwareAddr.String(),
			Addresses: encodedAddresses, SpeedBitsPerSecond: interfaceSpeedBits(sysClassNet, networkInterface.Name),
			MTU: networkInterface.MTU,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, nil
}

func interfaceSpeedBits(sysClassNet, name string) uint64 {
	if filepath.Base(name) != name {
		return 0
	}
	megabits, err := strconv.ParseInt(strings.TrimSpace(readText(filepath.Join(sysClassNet, name, "speed"))), 10, 64)
	if err != nil || megabits <= 0 {
		return 0
	}
	return uint64(megabits) * 1_000_000
}

func readText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func distributionName(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), "=")
		if found {
			values[key] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	if values["PRETTY_NAME"] != "" {
		return values["PRETTY_NAME"]
	}
	return values["ID"]
}

func linuxMemoryBytes(path string) int64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kilobytes, err := strconv.ParseInt(fields[1], 10, 64)
			if err == nil {
				return kilobytes * 1024
			}
		}
	}
	return 0
}
