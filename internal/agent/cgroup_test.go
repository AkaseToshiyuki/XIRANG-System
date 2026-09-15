package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
)

func TestCgroupManagerEnforcesCPUAndMemory(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{
		"cgroup.controllers": "cpu memory cpuset pids", "cgroup.subtree_control": "", "cgroup.procs": "",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := OpenCgroupManager(root)
	if err != nil {
		t.Fatal(err)
	}
	group, err := manager.Prepare("assignment-test", 2, &xirangv1.ResourceRequest{
		MilliCpu: 1500, MemoryBytes: 768 << 20, CpuSet: "2-3", NumaNodes: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertFileText(t, filepath.Join(group.path, "cpu.max"), "150000 100000")
	assertFileText(t, filepath.Join(group.path, "memory.max"), strconv.FormatUint(768<<20, 10))
	assertFileText(t, filepath.Join(group.path, "cpuset.cpus"), "2-3")
	assertFileText(t, filepath.Join(group.path, "cpuset.mems"), "0")
	if err := group.AddProcess(4321); err != nil {
		t.Fatal(err)
	}
	assertFileText(t, filepath.Join(group.path, "cgroup.procs"), "4321")
	if err := group.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(group.path); !os.IsNotExist(err) {
		t.Fatalf("assignment cgroup was not removed: %v", err)
	}
}

func TestCgroupManagerRejectsUnavailableController(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCgroupManager(root); err == nil || !strings.Contains(err.Error(), "memory controller") {
		t.Fatalf("expected missing memory controller, got %v", err)
	}
}

func TestExecutorLauncherWaitsForAdmissionGate(t *testing.T) {
	if os.Getenv("XIRANG_TEST_EXECUTOR_LAUNCHER") == "1" {
		if err := RunExecutorLauncher([]string{"/bin/sh", "-c", "printf launched > \"$1\"", "launcher", os.Getenv("XIRANG_TEST_MARKER")}); err != nil {
			os.Exit(125)
		}
		return
	}
	marker := filepath.Join(t.TempDir(), "launched")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestExecutorLauncherWaitsForAdmissionGate$")
	command.Env = append(os.Environ(), "XIRANG_TEST_EXECUTOR_LAUNCHER=1", "XIRANG_TEST_MARKER="+marker)
	command.ExtraFiles = []*os.File{reader}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("artifact ran before cgroup admission: %v", err)
	}
	if _, err := writer.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	assertFileText(t, marker, "launched")
}

func assertFileText(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s=%q, expected %q", path, data, expected)
	}
}
