//go:build linux

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

type deviceRule struct {
	deviceType uint32
	major      uint32
	minor      uint32
	anyMinor   bool
}

type deviceAuthorization struct {
	program *ebpf.Program
	link    link.Link
}

func (authorization *deviceAuthorization) Close() error {
	if authorization == nil {
		return nil
	}
	var result []error
	if authorization.link != nil {
		result = append(result, authorization.link.Close())
	}
	if authorization.program != nil {
		result = append(result, authorization.program.Close())
	}
	return errors.Join(result...)
}

func probeDeviceEnforcement(cgroupRoot string) error {
	path, err := os.MkdirTemp(cgroupRoot, ".xirang-device-probe-")
	if err != nil {
		return err
	}
	defer os.Remove(path)
	program, err := newDeviceProgram(baselineDeviceRules())
	if err != nil {
		return err
	}
	defer program.Close()
	attached, err := link.AttachCgroup(link.CgroupOptions{
		Path: path, Attach: ebpf.AttachCGroupDevice, Program: program,
	})
	if err != nil {
		return err
	}
	return attached.Close()
}

func (group *assignmentCgroup) authorizeDevices(requested map[string]int64, numaNodes string) (map[string][]string, error) {
	allowedNUMA, err := parseLinuxIDList(numaNodes)
	if err != nil {
		return nil, err
	}
	inventory, err := CollectInventory()
	if err != nil {
		return nil, fmt.Errorf("collect device inventory for authorization: %w", err)
	}
	selected := make(map[string][]string, len(requested))
	rules := baselineDeviceRules()
	for resource, quantity := range requested {
		if quantity <= 0 || quantity > 1_000_000 {
			return nil, fmt.Errorf("invalid requested device quantity for %s", resource)
		}
		for _, device := range inventory.Devices {
			if device.ResourceName != resource || int64(len(selected[resource])) >= quantity {
				continue
			}
			if len(allowedNUMA) > 0 {
				node, parseErr := strconv.ParseUint(device.Attributes["numaNode"], 10, 32)
				if parseErr != nil || !allowedNUMA[uint32(node)] {
					continue
				}
			}
			path := filepath.Clean(device.Attributes["devicePath"])
			if !strings.HasPrefix(path, "/dev/") {
				continue
			}
			if groupManagerDevicePathAllowed(group, resource, path) == false {
				continue
			}
			rule, statErr := deviceRuleForPath(path)
			if statErr != nil {
				continue
			}
			rules = append(rules, rule)
			selected[resource] = append(selected[resource], path)
		}
		if int64(len(selected[resource])) != quantity {
			return nil, fmt.Errorf("requested %d %s devices but only %d enforceable devices match", quantity, resource, len(selected[resource]))
		}
	}
	if len(selected["nvidia.com/gpu"]) > 0 {
		// The proprietary NVIDIA driver probes every numbered GPU device before
		// applying CUDA_VISIBLE_DEVICES. Returning EPERM for an unselected but
		// present /dev/nvidiaN makes CUDA initialization fail on multi-GPU
		// hosts. Authorize the numbered peers for driver discovery and keep the
		// scheduler-selected set in CUDA_VISIBLE_DEVICES/XIRANG_ASSIGNED_DEVICES.
		// Strong per-GPU isolation will require a device mount namespace in
		// addition to the cgroup filter.
		for _, device := range inventory.Devices {
			if device.ResourceName != "nvidia.com/gpu" {
				continue
			}
			path := filepath.Clean(device.Attributes["devicePath"])
			if !groupManagerDevicePathAllowed(group, "nvidia.com/gpu", path) {
				continue
			}
			if rule, ruleErr := deviceRuleForPath(path); ruleErr == nil {
				rules = append(rules, rule)
			}
		}
		paths := []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia-modeset"}
		// Recent proprietary drivers expose capability nodes separately from
		// the GPU and UVM devices. CUDA may inspect these during initialization
		// even when MIG is disabled, so include the existing capability nodes;
		// normal Unix ownership and mode checks still apply inside the task.
		if capabilityPaths, globErr := filepath.Glob("/dev/nvidia-caps/*"); globErr == nil {
			paths = append(paths, capabilityPaths...)
		}
		for _, path := range paths {
			if rule, err := deviceRuleForPath(path); err == nil {
				rules = append(rules, rule)
			}
		}
	}
	rules = uniqueDeviceRules(rules)
	program, err := newDeviceProgram(rules)
	if err != nil {
		return nil, fmt.Errorf("load cgroup device authorization: %w", err)
	}
	attached, err := link.AttachCgroup(link.CgroupOptions{
		Path: group.path, Attach: ebpf.AttachCGroupDevice, Program: program,
	})
	if err != nil {
		program.Close()
		return nil, fmt.Errorf("attach cgroup device authorization: %w", err)
	}
	group.deviceLink = &deviceAuthorization{program: program, link: attached}
	return selected, nil
}

func groupManagerDevicePathAllowed(group *assignmentCgroup, resource, path string) bool {
	if group == nil || group.manager == nil {
		return false
	}
	return group.manager.devicePaths[resource][path]
}

func baselineDeviceRules() []deviceRule {
	return []deviceRule{
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 1, minor: 3},
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 1, minor: 5},
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 1, minor: 7},
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 1, minor: 8},
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 1, minor: 9},
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 5, minor: 0},
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 5, minor: 2},
		{deviceType: unix.BPF_DEVCG_DEV_CHAR, major: 136, anyMinor: true},
	}
}

func deviceRuleForPath(path string) (deviceRule, error) {
	info, err := os.Stat(path)
	if err != nil {
		return deviceRule{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return deviceRule{}, errors.New("device metadata is unavailable")
	}
	deviceType := uint32(unix.BPF_DEVCG_DEV_CHAR)
	if info.Mode()&os.ModeDevice == 0 {
		return deviceRule{}, errors.New("path is not a device")
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		deviceType = uint32(unix.BPF_DEVCG_DEV_BLOCK)
	}
	return deviceRule{
		deviceType: deviceType, major: uint32(unix.Major(uint64(stat.Rdev))), minor: uint32(unix.Minor(uint64(stat.Rdev))),
	}, nil
}

func uniqueDeviceRules(rules []deviceRule) []deviceRule {
	seen := make(map[deviceRule]bool, len(rules))
	result := make([]deviceRule, 0, len(rules))
	for _, rule := range rules {
		if !seen[rule] {
			seen[rule] = true
			result = append(result, rule)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].deviceType != result[j].deviceType {
			return result[i].deviceType < result[j].deviceType
		}
		if result[i].major != result[j].major {
			return result[i].major < result[j].major
		}
		if result[i].anyMinor != result[j].anyMinor {
			return result[i].anyMinor
		}
		return result[i].minor < result[j].minor
	})
	return result
}

func newDeviceProgram(rules []deviceRule) (*ebpf.Program, error) {
	instructions := asm.Instructions{
		asm.LoadMem(asm.R2, asm.R1, 4, asm.Word),
		asm.LoadMem(asm.R3, asm.R1, 8, asm.Word),
		asm.LoadMem(asm.R4, asm.R1, 0, asm.Word),
		asm.And.Imm(asm.R4, 0xffff),
	}
	for index, rule := range rules {
		next := fmt.Sprintf("device_rule_%d", index)
		instructions = append(instructions,
			asm.JNE.Imm(asm.R4, int32(rule.deviceType), next),
			asm.JNE.Imm(asm.R2, int32(rule.major), next),
		)
		if rule.anyMinor {
			instructions = append(instructions, asm.Ja.Label("allow"))
		} else {
			instructions = append(instructions, asm.JEq.Imm(asm.R3, int32(rule.minor), "allow"))
		}
		instructions = append(instructions, asm.Mov.Imm(asm.R0, 0).WithSymbol(next))
	}
	instructions = append(instructions,
		asm.Mov.Imm(asm.R0, 0),
		asm.Return(),
		asm.Mov.Imm(asm.R0, 1).WithSymbol("allow"),
		asm.Return(),
	)
	return ebpf.NewProgramWithOptions(&ebpf.ProgramSpec{
		Name: "xirang_devices", Type: ebpf.CGroupDevice, AttachType: ebpf.AttachCGroupDevice,
		Instructions: instructions, License: "GPL",
	}, ebpf.ProgramOptions{LogLevel: ebpf.LogLevelInstruction, LogSizeStart: 1 << 20})
}

func parseLinuxIDList(value string) (map[uint32]bool, error) {
	result := make(map[uint32]bool)
	if value == "" {
		return result, nil
	}
	if !validCPUList(value) {
		return nil, errors.New("invalid Linux ID list")
	}
	for _, part := range strings.Split(value, ",") {
		startText, endText, found := strings.Cut(part, "-")
		start, err := strconv.ParseUint(startText, 10, 32)
		if err != nil {
			return nil, errors.New("invalid Linux ID list")
		}
		end := start
		if found {
			end, err = strconv.ParseUint(endText, 10, 32)
			if err != nil || end < start || end-start > 1_000_000 {
				return nil, errors.New("invalid Linux ID list range")
			}
		}
		for current := start; current <= end; current++ {
			result[uint32(current)] = true
		}
	}
	return result, nil
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
