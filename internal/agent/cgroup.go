package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"golang.org/x/sys/unix"
)

const cgroupPeriodMicros int64 = 100_000

// CgroupManager owns one delegated cgroup v2 subtree. The Agent process is
// moved into a management child before controllers are enabled for job siblings.
type CgroupManager struct {
	root              string
	cpuset            bool
	deviceEnforcement bool
	deviceReason      string
	devicePaths       map[string]map[string]bool
	mu                sync.Mutex
}

type assignmentCgroup struct {
	path            string
	manager         *CgroupManager
	deviceLink      io.Closer
	selectedDevices map[string][]string
}

func OpenCgroupManager(root string) (*CgroupManager, error) {
	if root == "auto" {
		var err error
		root, err = currentCgroupV2Path()
		if err != nil {
			return nil, err
		}
	}
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("executor cgroup root must be an absolute clean path")
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		parent := filepath.Dir(root)
		controllers, readErr := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
		if readErr != nil {
			return nil, fmt.Errorf("read parent cgroup v2 controllers: %w", readErr)
		}
		if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte(controllerEnableText(string(controllers))), 0o644); err != nil {
			return nil, fmt.Errorf("enable parent CPU and memory controllers: %w", err)
		}
		if err := os.Mkdir(root, 0o755); err != nil {
			return nil, fmt.Errorf("create delegated executor cgroup: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("inspect executor cgroup root: %w", err)
	}
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("read cgroup v2 controllers: %w", err)
	}
	available := " " + strings.TrimSpace(string(controllers)) + " "
	for _, controller := range []string{"cpu", "memory"} {
		if !strings.Contains(available, " "+controller+" ") {
			return nil, fmt.Errorf("cgroup v2 %s controller is unavailable under %s", controller, root)
		}
	}
	management := filepath.Join(root, "xirang-agent")
	if err := os.Mkdir(management, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create Agent management cgroup: %w", err)
	}
	if err := os.WriteFile(filepath.Join(management, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, fmt.Errorf("move Agent into management cgroup: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte(controllerEnableText(string(controllers))), 0o644); err != nil {
		return nil, fmt.Errorf("enable delegated CPU and memory controllers: %w", err)
	}
	manager := &CgroupManager{root: root, cpuset: strings.Contains(available, " cpuset ")}
	if fileSystemType(root, unix.CGROUP2_SUPER_MAGIC) {
		if probeErr := probeDeviceEnforcement(root); probeErr == nil {
			manager.deviceEnforcement = true
		} else {
			manager.deviceReason = probeErr.Error()
		}
	} else {
		manager.deviceReason = "executor root is not a cgroup v2 filesystem"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list delegated executor cgroup: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "job-") {
			continue
		}
		stale := &assignmentCgroup{path: filepath.Join(root, entry.Name())}
		if err := stale.KillAll(); err != nil {
			return nil, fmt.Errorf("clean stale assignment cgroup %s: %w", entry.Name(), err)
		}
		if err := stale.Cleanup(); err != nil {
			return nil, fmt.Errorf("remove stale assignment cgroup %s: %w", entry.Name(), err)
		}
	}
	return manager, nil
}

func controllerEnableText(controllers string) string {
	available := " " + strings.TrimSpace(controllers) + " "
	values := []string{"+cpu", "+memory"}
	if strings.Contains(available, " cpuset ") {
		values = append(values, "+cpuset")
	}
	return strings.Join(values, " ")
}

func currentCgroupV2Path() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read current cgroup membership: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "0::/") && line != "0::/" {
			continue
		}
		relative := strings.TrimPrefix(line, "0::")
		cleaned := filepath.Clean(relative)
		if !filepath.IsAbs(cleaned) || strings.Contains(cleaned, "..") {
			return "", errors.New("current cgroup v2 path is invalid")
		}
		return filepath.Join("/sys/fs/cgroup", cleaned), nil
	}
	return "", errors.New("unified cgroup v2 membership is unavailable")
}

func (manager *CgroupManager) SupportsCPUSet() bool {
	return manager != nil && manager.cpuset
}

func (manager *CgroupManager) SupportsDeviceEnforcement() bool {
	return manager != nil && manager.deviceEnforcement
}

func (manager *CgroupManager) DeviceEnforcementReason() string {
	if manager == nil {
		return "cgroup manager is unavailable"
	}
	return manager.deviceReason
}

func (manager *CgroupManager) ConfigureDeviceAccess(credential *syscall.Credential) []string {
	if manager == nil || !manager.deviceEnforcement || credential == nil {
		return nil
	}
	inventory, err := CollectInventory()
	if err != nil {
		return nil
	}
	paths := make(map[string]map[string]bool)
	for _, device := range inventory.Devices {
		path := filepath.Clean(device.Attributes["devicePath"])
		if !strings.HasPrefix(path, "/dev/") || !credentialCanAccess(path, credential) {
			continue
		}
		if paths[device.ResourceName] == nil {
			paths[device.ResourceName] = make(map[string]bool)
		}
		paths[device.ResourceName][path] = true
	}
	if len(paths["nvidia.com/gpu"]) > 0 && !credentialCanAccess("/dev/nvidiactl", credential) {
		delete(paths, "nvidia.com/gpu")
	}
	manager.devicePaths = paths
	resources := make([]string, 0, len(paths))
	for resource := range paths {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	return resources
}

func credentialCanAccess(path string, credential *syscall.Credential) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeDevice == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	permissions := info.Mode().Perm()
	if uint32(stat.Uid) == credential.Uid {
		return permissions&0o600 == 0o600
	}
	groups := append([]uint32{credential.Gid}, credential.Groups...)
	for _, group := range groups {
		if uint32(stat.Gid) == group {
			return permissions&0o060 == 0o060
		}
	}
	return permissions&0o006 == 0o006
}

func (manager *CgroupManager) Prepare(assignmentID string, attempt uint32, resources *xirangv1.ResourceRequest) (*assignmentCgroup, error) {
	if manager == nil || resources == nil || resources.MilliCpu <= 0 || resources.MemoryBytes == 0 {
		return nil, errors.New("positive CPU and memory cgroup limits are required")
	}
	if (resources.CpuSet != "" || resources.NumaNodes != "") && !manager.cpuset {
		return nil, errors.New("cpuset controller is unavailable")
	}
	if len(resources.Devices) != 0 && !manager.deviceEnforcement {
		return nil, errors.New("cgroup device enforcement is unavailable")
	}
	if resources.MilliCpu > (1<<63-1)/cgroupPeriodMicros {
		return nil, errors.New("CPU cgroup limit is too large")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	name := "job-" + workspaceName(assignmentID, attempt)[:24]
	path := filepath.Join(manager.root, name)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create assignment cgroup: %w", err)
	}
	quota := resources.MilliCpu * cgroupPeriodMicros / 1000
	if quota < 1000 {
		_ = removeCgroup(path)
		return nil, errors.New("CPU cgroup limit must be at least 10 millicores")
	}
	if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(fmt.Sprintf("%d %d", quota, cgroupPeriodMicros)), 0o644); err != nil {
		_ = removeCgroup(path)
		return nil, fmt.Errorf("configure assignment CPU quota: %w", err)
	}
	if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(strconv.FormatUint(resources.MemoryBytes, 10)), 0o644); err != nil {
		_ = removeCgroup(path)
		return nil, fmt.Errorf("configure assignment memory limit: %w", err)
	}
	if _, err := os.Stat(filepath.Join(path, "memory.swap.max")); err == nil {
		if err := os.WriteFile(filepath.Join(path, "memory.swap.max"), []byte("0"), 0o644); err != nil {
			_ = removeCgroup(path)
			return nil, fmt.Errorf("disable assignment swap: %w", err)
		}
	}
	if resources.NumaNodes != "" {
		if !validCPUList(resources.NumaNodes) {
			_ = removeCgroup(path)
			return nil, errors.New("invalid NUMA node list")
		}
		if err := os.WriteFile(filepath.Join(path, "cpuset.mems"), []byte(resources.NumaNodes), 0o644); err != nil {
			_ = removeCgroup(path)
			return nil, fmt.Errorf("configure assignment NUMA nodes: %w", err)
		}
	}
	if resources.CpuSet != "" {
		if !validCPUList(resources.CpuSet) {
			_ = removeCgroup(path)
			return nil, errors.New("invalid CPU set")
		}
		if err := os.WriteFile(filepath.Join(path, "cpuset.cpus"), []byte(resources.CpuSet), 0o644); err != nil {
			_ = removeCgroup(path)
			return nil, fmt.Errorf("configure assignment CPU set: %w", err)
		}
	}
	group := &assignmentCgroup{path: path, manager: manager}
	if len(resources.Devices) != 0 {
		selected, err := group.authorizeDevices(resources.Devices, resources.NumaNodes)
		if err != nil {
			_ = removeCgroup(path)
			return nil, err
		}
		group.selectedDevices = selected
	}
	return group, nil
}

func (group *assignmentCgroup) AddProcess(pid int) error {
	if group == nil || pid <= 0 {
		return errors.New("assignment cgroup and positive process ID are required")
	}
	if err := os.WriteFile(filepath.Join(group.path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("attach process to assignment cgroup: %w", err)
	}
	return nil
}

func (group *assignmentCgroup) CPUUsageMicros() (uint64, error) {
	if group == nil {
		return 0, nil
	}
	data, err := os.ReadFile(filepath.Join(group.path, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		return strconv.ParseUint(fields[1], 10, 64)
	}
	return 0, errors.New("cgroup cpu.stat does not contain usage_usec")
}

func (group *assignmentCgroup) MemoryCurrentBytes() (uint64, error) {
	if group == nil {
		return 0, nil
	}
	data, err := os.ReadFile(filepath.Join(group.path, "memory.current"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func (group *assignmentCgroup) Cleanup() error {
	if group == nil {
		return nil
	}
	var linkErr error
	if group.deviceLink != nil {
		linkErr = group.deviceLink.Close()
		group.deviceLink = nil
	}
	return errors.Join(linkErr, removeCgroup(group.path))
}

func (group *assignmentCgroup) KillAll() error {
	if group == nil {
		return nil
	}
	killPath := filepath.Join(group.path, "cgroup.kill")
	if _, err := os.Stat(killPath); err == nil {
		if err := os.WriteFile(killPath, []byte("1"), 0o644); err != nil {
			return fmt.Errorf("kill assignment cgroup: %w", err)
		}
	} else {
		data, readErr := os.ReadFile(filepath.Join(group.path, "cgroup.procs"))
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("read assignment cgroup processes: %w", readErr)
		}
		for _, field := range strings.Fields(string(data)) {
			pid, parseErr := strconv.Atoi(field)
			if parseErr == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	for attempt := 0; attempt < 100; attempt++ {
		data, err := os.ReadFile(filepath.Join(group.path, "cgroup.events"))
		if errors.Is(err, os.ErrNotExist) || (err == nil && strings.Contains(string(data), "populated 0")) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read assignment cgroup state: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("assignment cgroup remained populated after kill")
}

func removeCgroup(path string) error {
	// Controller files are virtual on cgroupfs and cannot be unlinked. Removing
	// regular files makes the same implementation safely testable in a temp tree.
	entries, err := os.ReadDir(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			_ = os.Remove(filepath.Join(path, entry.Name()))
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
