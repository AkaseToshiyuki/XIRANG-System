package build

import (
	"context"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func discoverToolchainCapabilities(names []string) []domain.ToolchainCapability {
	capabilities := make([]domain.ToolchainCapability, 0, len(names))
	seen := make(map[string]bool)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		capability := domain.ToolchainCapability{Name: name, TargetArchitectures: []string{runtime.GOARCH}}
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		capability.Executables = append(capability.Executables, name)
		capability.Version = commandVersion(path)
		switch name {
		case "go":
			capability.TargetArchitectures = goLinuxTargets()
		case "cc", "gcc", "clang":
			cross := map[string][]string{
				"amd64":   {"x86_64-linux-gnu-gcc", "x86_64-linux-gnu-clang"},
				"arm64":   {"aarch64-linux-gnu-gcc", "aarch64-linux-gnu-clang"},
				"riscv64": {"riscv64-linux-gnu-gcc", "riscv64-linux-gnu-clang"},
			}
			for architecture, candidates := range cross {
				for _, executable := range candidates {
					if _, err := exec.LookPath(executable); err == nil {
						capability.TargetArchitectures = append(capability.TargetArchitectures, architecture)
						capability.Executables = append(capability.Executables, executable)
						break
					}
				}
			}
		}
		capability.TargetArchitectures = uniqueSorted(capability.TargetArchitectures)
		capability.Executables = uniqueSorted(capability.Executables)
		capabilities = append(capabilities, capability)
	}
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i].Name < capabilities[j].Name })
	return capabilities
}

func goLinuxTargets() []string {
	path, err := exec.LookPath("go")
	if err != nil {
		return []string{runtime.GOARCH}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "tool", "dist", "list").Output()
	if err != nil {
		return []string{runtime.GOARCH}
	}
	var targets []string
	for _, item := range strings.Fields(string(output)) {
		if architecture, ok := strings.CutPrefix(item, "linux/"); ok {
			targets = append(targets, architecture)
		}
	}
	return uniqueSorted(targets)
}

func commandVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil && len(output) == 0 {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	if len(line) > 160 {
		line = line[:160]
	}
	return line
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func workerSupports(worker domain.BuildWorker, recipe domain.BuildRecipe) bool {
	for _, capability := range worker.ToolchainCapabilities {
		if capability.Name != recipe.Toolchain || !contains(capability.TargetArchitectures, recipe.Architecture) {
			continue
		}
		return true
	}
	// Older Agents did not report a matrix. Preserve native builds, but do not
	// claim cross-architecture support without positive capability evidence.
	return contains(worker.Toolchains, recipe.Toolchain) && recipe.Architecture == worker.Architecture
}
