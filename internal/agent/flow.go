package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultFlowCgroupPath = "/sys/fs/cgroup"
	defaultFlowMapEntries = 4096
	defaultFlowReportSize = 256
)

// FlowMonitoringConfig controls the optional eBPF network-flow observer. It is
// intentionally opt-in: normal resource and interface telemetry never depends
// on eBPF support or elevated privileges.
type FlowMonitoringConfig struct {
	Enabled    bool
	CgroupPath string
	MapEntries uint32
	ReportSize int
	Interval   time.Duration
}

// NetworkFlow is a payload-free, direction-aware aggregate for one IP 5-tuple.
type NetworkFlow struct {
	Family             string `json:"family"`
	Protocol           string `json:"protocol"`
	SourceAddress      string `json:"sourceAddress"`
	SourcePort         uint16 `json:"sourcePort"`
	DestinationAddress string `json:"destinationAddress"`
	DestinationPort    uint16 `json:"destinationPort"`
	Direction          string `json:"direction"`
	Packets            uint64 `json:"packets"`
	Bytes              uint64 `json:"bytes"`
}

// FlowSample contains deltas since the previous snapshot. An initial sample
// reports the map's current counters and is therefore useful for diagnostics.
type FlowSample struct {
	Flows      []NetworkFlow `json:"flows"`
	Truncated  bool          `json:"truncated"`
	MapEntries uint32        `json:"mapEntries"`
	Interval   time.Duration `json:"interval"`
}

// EBPFCapability reports prerequisites without changing kernel state.
type EBPFCapability struct {
	Supported       bool   `json:"supported"`
	OperatingSystem string `json:"operatingSystem"`
	Architecture    string `json:"architecture"`
	CgroupPath      string `json:"cgroupPath"`
	CgroupV2        bool   `json:"cgroupV2"`
	KernelBTF       bool   `json:"kernelBtf"`
	BPFFileSystem   bool   `json:"bpfFileSystem"`
	Privileged      bool   `json:"privileged"`
	Reason          string `json:"reason,omitempty"`
}

func normalizeFlowMonitoringConfig(config FlowMonitoringConfig) (FlowMonitoringConfig, error) {
	if config.CgroupPath == "" {
		config.CgroupPath = defaultFlowCgroupPath
	}
	if config.MapEntries == 0 {
		config.MapEntries = defaultFlowMapEntries
	}
	if config.MapEntries < 64 || config.MapEntries > 1<<20 {
		return config, errors.New("eBPF flow map entries must be between 64 and 1048576")
	}
	if config.ReportSize == 0 {
		config.ReportSize = defaultFlowReportSize
	}
	if config.ReportSize < 1 || config.ReportSize > 4096 {
		return config, errors.New("eBPF flow report size must be between 1 and 4096")
	}
	if config.Interval == 0 {
		config.Interval = 10 * time.Second
	}
	if config.Interval < time.Second || config.Interval > 5*time.Minute {
		return config, errors.New("eBPF flow interval must be between 1s and 5m")
	}
	return config, nil
}

// ProbeEBPFCapability performs filesystem and identity checks only. Actual BPF
// program loading remains the authoritative test because Linux capabilities,
// LSM policy, and service sandboxing can differ for the running Agent.
func ProbeEBPFCapability(cgroupPath string) EBPFCapability {
	if cgroupPath == "" {
		cgroupPath = defaultFlowCgroupPath
	}
	result := EBPFCapability{
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
		CgroupPath:      cgroupPath,
		Privileged:      os.Geteuid() == 0,
	}
	if runtime.GOOS != "linux" {
		result.Reason = "eBPF flow monitoring is available only on Linux"
		return result
	}
	result.CgroupV2 = fileSystemType(cgroupPath, unix.CGROUP2_SUPER_MAGIC)
	result.BPFFileSystem = fileSystemType("/sys/fs/bpf", unix.BPF_FS_MAGIC)
	if info, err := os.Stat("/sys/kernel/btf/vmlinux"); err == nil && info.Mode().IsRegular() {
		result.KernelBTF = true
	}
	if !result.CgroupV2 {
		result.Reason = "missing cgroup v2"
		return result
	}
	if !result.Privileged {
		result.Reason = "cgroup v2 is present; loading still requires CAP_BPF/CAP_NET_ADMIN or root"
		return result
	}
	result.Supported = true
	return result
}

func fileSystemType(path string, expected int64) bool {
	var stat unix.Statfs_t
	return unix.Statfs(path, &stat) == nil && int64(stat.Type) == expected
}

func sortedFlowDeltas(current, previous map[flowKey]flowValue, limit int) ([]NetworkFlow, bool) {
	flows := make([]NetworkFlow, 0, len(current))
	for key, value := range current {
		prior := previous[key]
		packets := counterDelta(value.Packets, prior.Packets)
		bytes := counterDelta(value.Bytes, prior.Bytes)
		if packets == 0 && bytes == 0 {
			continue
		}
		flow, ok := decodeFlow(key, packets, bytes)
		if ok {
			flows = append(flows, flow)
		}
	}
	sort.Slice(flows, func(i, j int) bool {
		if flows[i].Bytes != flows[j].Bytes {
			return flows[i].Bytes > flows[j].Bytes
		}
		if flows[i].Packets != flows[j].Packets {
			return flows[i].Packets > flows[j].Packets
		}
		left, _ := json.Marshal(flows[i])
		right, _ := json.Marshal(flows[j])
		return string(left) < string(right)
	})
	truncated := len(flows) > limit
	if truncated {
		flows = flows[:limit]
	}
	return flows, truncated
}

func counterDelta(current, previous uint64) uint64 {
	if current >= previous {
		return current - previous
	}
	// An LRU eviction followed by reinsertion restarts the counter.
	return current
}

func decodeFlow(key flowKey, packets, bytes uint64) (NetworkFlow, bool) {
	var source, destination netip.Addr
	var family string
	switch key.Family {
	case flowFamilyIPv4:
		family = "ipv4"
		source = netip.AddrFrom4([4]byte(key.Source[12:16]))
		destination = netip.AddrFrom4([4]byte(key.Destination[12:16]))
	case flowFamilyIPv6:
		family = "ipv6"
		source = netip.AddrFrom16(key.Source)
		destination = netip.AddrFrom16(key.Destination)
	default:
		return NetworkFlow{}, false
	}
	protocol := map[uint8]string{6: "tcp", 17: "udp"}[key.Protocol]
	direction := map[uint8]string{flowDirectionIngress: "ingress", flowDirectionEgress: "egress"}[key.Direction]
	if protocol == "" || direction == "" {
		return NetworkFlow{}, false
	}
	return NetworkFlow{
		Family: family, Protocol: protocol,
		SourceAddress: source.String(), SourcePort: networkPort(key.SourcePort),
		DestinationAddress: destination.String(), DestinationPort: networkPort(key.DestinationPort),
		Direction: direction, Packets: packets, Bytes: bytes,
	}, true
}

func networkPort(value [2]byte) uint16 {
	return uint16(value[0])<<8 | uint16(value[1])
}

func flowCollectorError(operation string, err error) error {
	return fmt.Errorf("%s: %w", operation, err)
}
