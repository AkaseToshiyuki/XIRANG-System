package jobspec

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	spec, err := Parse(strings.NewReader(`
XIRANG 1
artifact = https://controller/artifacts/demo
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = arm64
entrypoint = bin/demo
cpu = 2
memory = 4G
device.nvidia.com/gpu = 1
timeout = 2h
env.MODE = production
arg = --fast
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Resources.MilliCPU != 2000 || spec.Resources.MemoryBytes != 4<<30 ||
		spec.Resources.Devices["nvidia.com/gpu"] != 1 || spec.Environment["MODE"] != "production" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
}

func TestParseApplicationBatchManifest(t *testing.T) {
	spec, err := Parse(strings.NewReader(`
XIRANG 1
application = gromacs
cpu = 8
memory = 16G
device.nvidia.com/gpu = 1
batch_space = share_default
batch_file = inputs/gromacs.txt
batch_arg = -s
batch_nodes = 4
arg = mdrun
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Application != "gromacs" || spec.BatchNodes != 4 || spec.BatchPath != "inputs/gromacs.txt" || spec.BatchArgument != "-s" {
		t.Fatalf("unexpected batch application spec: %#v", spec)
	}
}

func TestParseRejectsBatchArrayCombination(t *testing.T) {
	_, err := Parse(strings.NewReader(`
XIRANG 1
application = gromacs
cpu = 1
memory = 1G
array = 0-3
batch_space = share_default
batch_file = inputs.txt
batch_nodes = 2
`))
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected batch/array conflict, got %v", err)
	}
}

func TestParseLongDayTimeout(t *testing.T) {
	spec, err := Parse(strings.NewReader(`
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = 1
memory = 1G
timeout = 30d
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Timeout != 30*24*time.Hour {
		t.Fatalf("timeout = %s, want 30d", spec.Timeout)
	}
}

func TestParseRejectsUserStorageDeclaration(t *testing.T) {
	script := `
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = 1
memory = 1G
storage = 10G
`
	if _, err := Parse(strings.NewReader(script)); err == nil || !strings.Contains(err.Error(), "managed by the administrator") {
		t.Fatalf("expected an administrator-managed storage error, got %v", err)
	}
}

func TestParseRejectsInvalidDeviceResource(t *testing.T) {
	script := `
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = 1
memory = 1G
device.nvidia.com/gpu = 0
`
	if _, err := Parse(strings.NewReader(script)); err == nil {
		t.Fatal("expected invalid device quantity to fail")
	}
}

func TestParseResourceUnits(t *testing.T) {
	tests := []struct {
		name        string
		cpu         string
		memory      string
		wantCPU     int64
		wantMemory  int64
		shouldError bool
	}{
		{name: "logical cores and megabytes", cpu: "8", memory: "512M", wantCPU: 8000, wantMemory: 512 << 20},
		{name: "logical cores and gigabytes", cpu: "96", memory: "504G", wantCPU: 96000, wantMemory: 504 << 30},
		{name: "legacy units remain readable", cpu: "500m", memory: "1GiB", wantCPU: 500, wantMemory: 1 << 30},
		{name: "fractional cores are rejected", cpu: "0.5", memory: "1G", shouldError: true},
		{name: "cpu suffix is rejected", cpu: "2core", memory: "1G", shouldError: true},
		{name: "capacity needs a unit", cpu: "2", memory: "1024", shouldError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := `
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = ` + test.cpu + `
memory = ` + test.memory
			spec, err := Parse(strings.NewReader(script))
			if test.shouldError {
				if err == nil {
					t.Fatal("expected resource declaration to fail")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if spec.Resources.MilliCPU != test.wantCPU || spec.Resources.MemoryBytes != test.wantMemory {
				t.Fatalf("resources = %#v, want cpu=%d memory=%d", spec.Resources, test.wantCPU, test.wantMemory)
			}
		})
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	if _, err := Parse(strings.NewReader("surprise = true")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseArrayAndDependencies(t *testing.T) {
	script := `
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = 1
memory = 1G
array = 3-5
depends = job_first
depends = job_cleanup:afterany
`
	spec, err := Parse(strings.NewReader(script))
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.ArrayIndices) != 3 || spec.ArrayIndices[0] != 3 || spec.ArrayIndices[2] != 5 ||
		len(spec.Dependencies) != 2 || spec.Dependencies[0].Condition != "afterok" ||
		spec.Dependencies[1].Condition != "afterany" {
		t.Fatalf("unexpected workflow fields: %#v", spec)
	}
}

func TestParseParallelDefaultsRemainSingleNodeCompatible(t *testing.T) {
	spec, err := Parse(strings.NewReader(`
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = 1
memory = 1G
`))
	if err != nil {
		t.Fatal(err)
	}
	effective := spec.Parallel.Effective()
	if effective.Nodes != 1 || effective.TasksPerNode != 1 || effective.Launcher != "native" || effective.WorldSize() != 1 {
		t.Fatalf("unexpected compatibility defaults: %+v", effective)
	}
}

func TestParseParallelJob(t *testing.T) {
	script := `
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = 1
memory = 1G
nodes = 3
tasks_per_node = 4
launcher = mpich
`
	spec, err := Parse(strings.NewReader(script))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Parallel.Nodes != 3 || spec.Parallel.TasksPerNode != 4 || spec.Parallel.Launcher != "mpich" || spec.Parallel.WorldSize() != 12 {
		t.Fatalf("unexpected parallel spec: %+v", spec.Parallel)
	}
}

func TestParseRejectsInvalidParallelJob(t *testing.T) {
	base := `
XIRANG 1
artifact = file:///tmp/test
sha256 = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
architecture = amd64
entrypoint = test
cpu = 1
memory = 1G
`
	for _, fields := range []string{
		"nodes = 0", "nodes = 129", "tasks_per_node = 0", "tasks_per_node = 257",
		"nodes = 128\ntasks_per_node = 256", "launcher = ssh",
	} {
		if _, err := Parse(strings.NewReader(base + fields + "\n")); err == nil {
			t.Fatalf("expected %q to be rejected", fields)
		}
	}
}
