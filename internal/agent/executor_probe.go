package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

type NativeExecutorProbeResult struct {
	Supported         bool                        `json:"supported"`
	TaskUID           string                      `json:"taskUid"`
	CgroupPath        string                      `json:"cgroupPath"`
	CPUMax            string                      `json:"cpuMax"`
	MemoryMax         string                      `json:"memoryMax"`
	CPUSet            string                      `json:"cpuSet,omitempty"`
	NUMANodes         string                      `json:"numaNodes,omitempty"`
	DeviceEnforcement bool                        `json:"deviceEnforcement"`
	DeviceReason      string                      `json:"deviceReason,omitempty"`
	DeviceResources   []string                    `json:"deviceResources,omitempty"`
	DeviceAuthorized  bool                        `json:"deviceAuthorized"`
	UnassignedDenied  bool                        `json:"unassignedDeviceDenied"`
	Architecture      string                      `json:"architecture"`
	Stress            *NativeExecutorStressResult `json:"stress,omitempty"`
}

type NativeExecutorStressResult struct {
	CPUThrottled bool `json:"cpuThrottled"`
	OOMKilled    bool `json:"oomKilled"`
	Canceled     bool `json:"canceled"`
}

func ProbeNativeExecutor(ctx context.Context, config ExecutorIsolationConfig) (NativeExecutorProbeResult, error) {
	stateDirectory, err := os.MkdirTemp("", "xirang-executor-probe-state-")
	if err != nil {
		return NativeExecutorProbeResult{}, err
	}
	defer os.RemoveAll(stateDirectory)
	if config.TaskDirectory == "" {
		config.TaskDirectory, err = os.MkdirTemp("", "xirang-executor-probe-tasks-")
		if err != nil {
			return NativeExecutorProbeResult{}, err
		}
		defer os.RemoveAll(config.TaskDirectory)
	}
	script := "#!/bin/sh\nset -eu\ncg=$(awk -F: '$1 == \"0\" { print $3 }' /proc/self/cgroup)\n" +
		"{ id -u; printf '%s\\n' \"$cg\"; cat /sys/fs/cgroup\"$cg\"/cpu.max; cat /sys/fs/cgroup\"$cg\"/memory.max; " +
		"cat /sys/fs/cgroup\"$cg\"/cpuset.cpus 2>/dev/null || printf -- '-\\n'; cat /sys/fs/cgroup\"$cg\"/cpuset.mems 2>/dev/null || printf -- '-\\n'; } > result.txt\n"
	artifact := filepath.Join(stateDirectory, "probe.sh")
	if err := os.WriteFile(artifact, []byte(script), 0o700); err != nil {
		return NativeExecutorProbeResult{}, err
	}
	executor, err := OpenIsolatedNativeExecutor(stateDirectory, config)
	if err != nil {
		return NativeExecutorProbeResult{}, err
	}
	cpuSet, numaNodes := "", ""
	if executor.SupportsCPUSet() {
		cpuSet = firstLinuxID(readText(filepath.Join(executor.cgroups.root, "cpuset.cpus.effective")))
		numaNodes = firstLinuxID(readText(filepath.Join(executor.cgroups.root, "cpuset.mems.effective")))
	}
	digest := sha256.Sum256([]byte(script))
	command := &xirangv1.RunAssignment{
		AssignmentId: "executor-probe", JobId: "executor-probe", Attempt: 1,
		Artifact: &xirangv1.NativeArtifact{
			Uri: "file://" + artifact, Sha256: fmt.Sprintf("%x", digest), Architecture: runtime.GOARCH, Entrypoint: "probe.sh",
		},
		Resources: &xirangv1.ResourceRequest{MilliCpu: 250, MemoryBytes: 128 << 20, CpuSet: cpuSet, NumaNodes: numaNodes},
		Timeout:   durationpb.New(10 * time.Second),
	}
	accepted, reason := executor.AcceptRun(ctx, command)
	if !accepted {
		return NativeExecutorProbeResult{}, fmt.Errorf("executor probe rejected: %s", reason)
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-executor.Updates():
			if update.State == xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED || update.State == xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED {
				return NativeExecutorProbeResult{}, fmt.Errorf("executor probe failed: %s", update.Reason)
			}
			if update.State != xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED {
				continue
			}
			resultPath := filepath.Join(config.TaskDirectory, workspaceName(command.AssignmentId, command.Attempt), "result.txt")
			data, err := os.ReadFile(resultPath)
			if err != nil {
				return NativeExecutorProbeResult{}, fmt.Errorf("read executor probe result: %w", err)
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if len(lines) != 6 {
				return NativeExecutorProbeResult{}, errors.New("executor probe returned malformed isolation data")
			}
			if lines[4] == "-" {
				lines[4] = ""
			}
			if lines[5] == "-" {
				lines[5] = ""
			}
			result := NativeExecutorProbeResult{
				Supported: true, TaskUID: lines[0], CgroupPath: lines[1], CPUMax: lines[2], MemoryMax: lines[3],
				CPUSet: lines[4], NUMANodes: lines[5], DeviceEnforcement: executor.cgroups.SupportsDeviceEnforcement(),
				DeviceResources: executor.DeviceResources(), DeviceReason: executor.cgroups.DeviceEnforcementReason(), Architecture: runtime.GOARCH,
			}
			if len(result.DeviceResources) > 0 {
				result.DeviceAuthorized, result.UnassignedDenied, err = probeDeviceAuthorization(ctx, executor, result.DeviceResources[0])
				if err != nil {
					return NativeExecutorProbeResult{}, err
				}
			}
			return result, nil
		case <-ctx.Done():
			return NativeExecutorProbeResult{}, ctx.Err()
		case <-timer.C:
			return NativeExecutorProbeResult{}, errors.New("executor probe timed out")
		}
	}
}

func probeDeviceAuthorization(ctx context.Context, executor *NativeExecutor, resource string) (bool, bool, error) {
	inventory, err := CollectInventory()
	if err != nil {
		return false, false, err
	}
	var paths []string
	for _, device := range inventory.Devices {
		path := filepath.Clean(device.Attributes["devicePath"])
		if device.ResourceName == resource && executor.cgroups.devicePaths[resource][path] {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return false, false, errors.New("device capability has no enforceable path")
	}
	executable, err := os.Executable()
	if err != nil {
		return false, false, err
	}
	artifact, err := os.ReadFile(executable)
	if err != nil {
		return false, false, err
	}
	unassigned := "-"
	// NVIDIA's proprietary userspace probes every numbered peer during CUDA
	// initialization, and the production authorization intentionally permits
	// those peers. Validate the selected node and shared control devices here;
	// strict peer denial is tested only for resource types that support it.
	if len(paths) > 1 && resource != "nvidia.com/gpu" {
		unassigned = paths[1]
	}
	command := &xirangv1.RunAssignment{
		AssignmentId: "executor-probe-device", JobId: "executor-probe-device", Attempt: 1,
		Artifact: &xirangv1.NativeArtifact{
			Uri: "file://" + executable, Sha256: fmt.Sprintf("%x", sha256.Sum256(artifact)),
			Architecture: runtime.GOARCH, Entrypoint: "xirang-agent",
		},
		Arguments: []string{"__executor-device-workload", paths[0], unassigned},
		Resources: &xirangv1.ResourceRequest{
			MilliCpu: 250, MemoryBytes: 128 << 20, Devices: map[string]int64{resource: 1},
		},
		Timeout: durationpb.New(10 * time.Second),
	}
	if accepted, reason := executor.AcceptRun(ctx, command); !accepted {
		return false, false, fmt.Errorf("device authorization probe rejected: %s", reason)
	}
	update, err := waitProbeAssignment(ctx, executor, command.AssignmentId, false)
	if err != nil {
		return false, false, err
	}
	if update.State != xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED {
		return false, false, fmt.Errorf("device authorization probe failed: %s stdout=%q stderr=%q", update.Reason, update.StdoutTail, update.StderrTail)
	}
	output := string(update.StdoutTail)
	return strings.Contains(output, "assigned-open=true"), strings.Contains(output, "unassigned-denied=true"), nil
}

func RunExecutorDeviceWorkload(assigned, unassigned string) error {
	assignedFile, err := os.OpenFile(assigned, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open assigned device: %w", err)
	}
	_ = assignedFile.Close()
	fmt.Println("assigned-open=true")
	if unassigned == "-" {
		fmt.Println("unassigned-denied=false")
		return nil
	}
	unassignedFile, err := os.OpenFile(unassigned, os.O_RDWR, 0)
	if err == nil {
		_ = unassignedFile.Close()
		return errors.New("unassigned device was accessible")
	}
	fmt.Println("unassigned-denied=true")
	return nil
}

func firstLinuxID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	first := strings.Split(value, ",")[0]
	first = strings.Split(first, "-")[0]
	if _, err := strconv.ParseUint(first, 10, 32); err != nil {
		return ""
	}
	return first
}

func ProbeNativeExecutorStress(ctx context.Context, config ExecutorIsolationConfig) (NativeExecutorStressResult, error) {
	stateDirectory, err := os.MkdirTemp("", "xirang-executor-stress-state-")
	if err != nil {
		return NativeExecutorStressResult{}, err
	}
	defer os.RemoveAll(stateDirectory)
	if config.TaskDirectory == "" {
		config.TaskDirectory, err = os.MkdirTemp("", "xirang-executor-stress-tasks-")
		if err != nil {
			return NativeExecutorStressResult{}, err
		}
		defer os.RemoveAll(config.TaskDirectory)
	}
	executor, err := OpenIsolatedNativeExecutor(stateDirectory, config)
	if err != nil {
		return NativeExecutorStressResult{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return NativeExecutorStressResult{}, err
	}
	artifact, err := os.ReadFile(executable)
	if err != nil {
		return NativeExecutorStressResult{}, err
	}
	digest := sha256.Sum256(artifact)
	command := func(id, workload string, resources *xirangv1.ResourceRequest) *xirangv1.RunAssignment {
		return &xirangv1.RunAssignment{
			AssignmentId: id, JobId: id, Attempt: 1,
			Artifact:  &xirangv1.NativeArtifact{Uri: "file://" + executable, Sha256: fmt.Sprintf("%x", digest), Architecture: runtime.GOARCH, Entrypoint: "xirang-agent"},
			Arguments: []string{"__executor-stress-workload", workload}, Resources: resources, Timeout: durationpb.New(15 * time.Second),
		}
	}
	cpu := command("executor-stress-cpu", "cpu", &xirangv1.ResourceRequest{MilliCpu: 100, MemoryBytes: 128 << 20})
	if accepted, reason := executor.AcceptRun(ctx, cpu); !accepted {
		return NativeExecutorStressResult{}, fmt.Errorf("CPU stress rejected: %s", reason)
	}
	cpuResult, err := waitProbeAssignment(ctx, executor, cpu.AssignmentId, false)
	if err != nil {
		return NativeExecutorStressResult{}, err
	}
	if cpuResult.State != xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED || !strings.Contains(string(cpuResult.StdoutTail), "cpu-throttled=true") {
		return NativeExecutorStressResult{}, fmt.Errorf("CPU throttle stress failed: state=%s reason=%s output=%q", cpuResult.State, cpuResult.Reason, cpuResult.StdoutTail)
	}
	oom := command("executor-stress-oom", "memory", &xirangv1.ResourceRequest{MilliCpu: 250, MemoryBytes: 64 << 20})
	if accepted, reason := executor.AcceptRun(ctx, oom); !accepted {
		return NativeExecutorStressResult{}, fmt.Errorf("OOM stress rejected: %s", reason)
	}
	oomResult, err := waitProbeAssignment(ctx, executor, oom.AssignmentId, false)
	if err != nil {
		return NativeExecutorStressResult{}, err
	}
	if oomResult.State != xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED || !strings.Contains(oomResult.Reason, "signal: killed") {
		return NativeExecutorStressResult{}, fmt.Errorf("OOM stress did not receive a cgroup kill: state=%s reason=%s", oomResult.State, oomResult.Reason)
	}
	cancel := command("executor-stress-cancel", "wait", &xirangv1.ResourceRequest{MilliCpu: 250, MemoryBytes: 128 << 20})
	if accepted, reason := executor.AcceptRun(ctx, cancel); !accepted {
		return NativeExecutorStressResult{}, fmt.Errorf("cancel stress rejected: %s", reason)
	}
	if _, err := waitProbeAssignment(ctx, executor, cancel.AssignmentId, true); err != nil {
		return NativeExecutorStressResult{}, err
	}
	if accepted, reason := executor.AcceptCancel(&xirangv1.CancelAssignment{AssignmentId: cancel.AssignmentId, GracePeriod: durationpb.New(0)}); !accepted {
		return NativeExecutorStressResult{}, fmt.Errorf("cancel stress rejected: %s", reason)
	}
	cancelResult, err := waitProbeAssignment(ctx, executor, cancel.AssignmentId, false)
	if err != nil {
		return NativeExecutorStressResult{}, err
	}
	if cancelResult.State != xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED {
		return NativeExecutorStressResult{}, fmt.Errorf("cancel stress failed: state=%s reason=%s", cancelResult.State, cancelResult.Reason)
	}
	return NativeExecutorStressResult{CPUThrottled: true, OOMKilled: true, Canceled: true}, nil
}

func waitProbeAssignment(ctx context.Context, executor *NativeExecutor, assignmentID string, running bool) (*xirangv1.AssignmentUpdate, error) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-executor.Updates():
			if update.AssignmentId != assignmentID {
				continue
			}
			if running && update.State == xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING {
				return update, nil
			}
			if !running && (update.State == xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED || update.State == xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED || update.State == xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED) {
				return update, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errors.New("executor stress assignment timed out")
		}
	}
}

func RunExecutorStressWorkload(kind string) error {
	switch kind {
	case "cpu":
		before := currentCgroupThrottleCount()
		deadline := time.Now().Add(1500 * time.Millisecond)
		value := uint64(1)
		for time.Now().Before(deadline) {
			value = value*1664525 + 1013904223
		}
		runtime.KeepAlive(value)
		after := currentCgroupThrottleCount()
		fmt.Printf("cpu-throttled=%t before=%d after=%d\n", after > before, before, after)
		if after <= before {
			return errors.New("CPU workload was not throttled")
		}
		return nil
	case "memory":
		memory := make([]byte, 512<<20)
		for index := 0; index < len(memory); index += os.Getpagesize() {
			memory[index] = 1
		}
		runtime.KeepAlive(memory)
		time.Sleep(10 * time.Second)
		return errors.New("memory workload unexpectedly survived")
	case "wait":
		time.Sleep(30 * time.Second)
		return nil
	default:
		return errors.New("unknown executor stress workload")
	}
}

func currentCgroupThrottleCount() uint64 {
	file, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	defer file.Close()
	path := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ":", 3)
		if len(parts) == 3 && parts[0] == "0" {
			path = parts[2]
			break
		}
	}
	data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "cpu.stat"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	for index := 0; index+1 < len(fields); index += 2 {
		if fields[index] == "nr_throttled" {
			value, _ := strconv.ParseUint(fields[index+1], 10, 64)
			return value
		}
	}
	return 0
}
