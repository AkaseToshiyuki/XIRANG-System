package agent

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDistributionNamePrefersPrettyName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	if err := os.WriteFile(path, []byte("ID=xirang\nPRETTY_NAME=\"XIRANG Test Linux\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := distributionName(path); got != "XIRANG Test Linux" {
		t.Fatalf("unexpected distribution: %q", got)
	}
}

func TestLinuxMemoryBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(path, []byte("MemTotal:       2048 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := linuxMemoryBytes(path); got != 2*1024*1024 {
		t.Fatalf("unexpected memory bytes: %d", got)
	}
}

func TestCollectAcceleratorsBuildsPortableResourceNames(t *testing.T) {
	dir := t.TempDir()
	drmRoot := filepath.Join(dir, "drm")
	nvidiaRoot := filepath.Join(dir, "nvidia")
	if err := os.MkdirAll(filepath.Join(drmRoot, "renderD128", "device"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drmRoot, "renderD128", "device", "vendor"), []byte("0x1002\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(nvidiaRoot, "0000:65:00.0"), 0o700); err != nil {
		t.Fatal(err)
	}

	devices := collectAccelerators(drmRoot, nvidiaRoot)
	if len(devices) != 2 {
		t.Fatalf("expected two accelerator resources, got %#v", devices)
	}
	if devices[0].ResourceName != "nvidia.com/gpu" || devices[0].Vendor != "nvidia" {
		t.Fatalf("unexpected NVIDIA inventory: %#v", devices[0])
	}
	if devices[1].ResourceName != "xirang.io/drm-render" || devices[1].Vendor != "amd" || devices[1].DeviceID != "renderD128" {
		t.Fatalf("unexpected DRM inventory: %#v", devices[1])
	}
}

func TestCollectNUMATopology(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, "node1")
	if err := os.MkdirAll(node, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "cpulist"), []byte("4-7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(node, "meminfo"), []byte("Node 1 MemTotal: 2048 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nodes := collectNUMATopology(root)
	if len(nodes) != 1 || nodes[0].ID != 1 || nodes[0].CPUs != "4-7" || nodes[0].MemoryBytes != 2*1024*1024 {
		t.Fatalf("unexpected NUMA topology: %#v", nodes)
	}
}

func TestNVIDIADeviceMinor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "information")
	if err := os.WriteFile(path, []byte("Model: Test GPU\nDevice Minor: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if minor := nvidiaDeviceMinor(path); minor != "7" {
		t.Fatalf("unexpected device minor %q", minor)
	}
	if model := nvidiaDeviceInformation(path)["Model"]; model != "Test GPU" {
		t.Fatalf("unexpected model %q", model)
	}
}

func TestParseCPUInfoCollectsPortableFeatureSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpuinfo")
	contents := "processor : 0\nmodel name : Test CPU\nflags : sse4_2 avx2 aes\n\nprocessor : 1\nflags : aes avx2\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	model, features := parseCPUInfo(path)
	if model != "Test CPU" || len(features) != 3 || features[0] != "aes" || features[2] != "sse4_2" {
		t.Fatalf("unexpected CPU capabilities model=%q features=%#v", model, features)
	}
}

func TestCUDACoresPerSMIsArchitectureAware(t *testing.T) {
	if got := cudaCoresPerSM("8.6"); got != 128 {
		t.Fatalf("Ampere SM core count = %d", got)
	}
	if got := cudaCoresPerSM("future"); got != 0 {
		t.Fatalf("unknown architecture must remain SM-count only, got %d", got)
	}
}

func TestKnownNVIDIAProfileQuantifiesSMCapacity(t *testing.T) {
	capability, smCount := knownNVIDIAProfile("NVIDIA GeForce RTX 3080", "0x2206")
	if capability != "8.6" || smCount != 68 || smCount*cudaCoresPerSM(capability) != 8704 {
		t.Fatalf("unexpected profile capability=%q sm=%d", capability, smCount)
	}
	capability, smCount = knownNVIDIAProfile("NVIDIA Corporation GP104GL [Tesla P4]", "0x1bb3")
	if capability != "6.1" || smCount != 20 || smCount*cudaCoresPerSM(capability) != 2560 {
		t.Fatalf("unexpected Tesla P4 profile capability=%q sm=%d", capability, smCount)
	}
}

func TestParseNVIDIASMIPortableTelemetryFields(t *testing.T) {
	output := []byte("00000000:01:00.0, NVIDIA GeForce RTX 3080, GPU-a, 8.6, 17, 43, 1024, 20480\n" +
		"00000000:02:00.0, NVIDIA GeForce RTX 3080, GPU-b, 8.6, N/A, 41, 0, 20480\n")
	gpus := parseNVIDIASMI(output, true)
	if len(gpus) != 2 {
		t.Fatalf("expected two GPUs, got %#v", gpus)
	}
	if gpus[0].PCIAddress != "0000:01:00.0" || !gpus[0].UtilizationAvailable ||
		!gpus[0].TemperatureAvailable || gpus[0].SMCount != 68 || gpus[0].CUDACores != 8704 {
		t.Fatalf("unexpected first GPU telemetry: %#v", gpus[0])
	}
	if gpus[1].UtilizationAvailable || !gpus[1].TemperatureAvailable {
		t.Fatalf("N/A availability was not preserved: %#v", gpus[1])
	}
}

func TestComputeWeightCombinesCPUAndNVIDIASMCapacity(t *testing.T) {
	devices := []DeviceInventory{
		{Attributes: map[string]string{"smComputeScore": "8704"}},
		{Attributes: map[string]string{"smComputeScore": "8704"}},
	}
	if weight := computeWeight(50.27, devices); weight != 67.68 {
		t.Fatalf("unexpected compute weight %.2f", weight)
	}
}

func TestComputeWeightKeepsCPUFallbackWithGPUProfile(t *testing.T) {
	devices := []DeviceInventory{{Attributes: map[string]string{"smComputeScore": "8704"}}}
	want := math.Round((float64(runtime.NumCPU())+8.704)*100) / 100
	if got := computeWeight(0, devices); got != want {
		t.Fatalf("computeWeight(0, GPU) = %.2f, want %.2f", got, want)
	}
}

func TestComputeWeightRejectsNonFiniteCalibration(t *testing.T) {
	devices := []DeviceInventory{{Attributes: map[string]string{"smComputeScore": "NaN"}}}
	if got, want := computeWeight(math.NaN(), devices), float64(runtime.NumCPU()); got != want {
		t.Fatalf("computeWeight(NaN, NaN) = %.2f, want %.2f", got, want)
	}
}

func TestCPUTemperatureAcceptsACPIThermalZoneFallback(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "hwmon0")
	if err := os.MkdirAll(device, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "name"), []byte("acpitz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "temp1_input"), []byte("48000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if temperature := collectCPUTemperature(root); temperature != 48 {
		t.Fatalf("unexpected ACPI temperature %.1f", temperature)
	}
}

func TestCPUTemperatureAcceptsGenericHwmonWithCPULabel(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "hwmon0")
	if err := os.MkdirAll(device, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"name": "vendor_monitor\n", "temp1_label": "CPU Package\n", "temp1_input": "55000\n",
		"temp2_label": "NVMe Composite\n", "temp2_input": "79000\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(device, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if temperature := collectCPUTemperature(root); temperature != 55 {
		t.Fatalf("unexpected labeled CPU temperature %.1f", temperature)
	}
}
