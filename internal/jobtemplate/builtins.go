package jobtemplate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/xirang-grid/xirang/internal/domain"
)

type builtinDefinition struct {
	name, description, artifact, architecture, body string
	// legacyName and legacyDescription are the Simplified Chinese strings these
	// templates shipped with up to 1.8.15-dev. They were written into every
	// cluster's database on first start, so the console rendered them verbatim
	// in both languages -- a Chinese description under an English heading. The
	// text is kept here so relabelLegacyBuiltins can recognise an untouched row
	// and move it to the English name the console now localises from.
	legacyName, legacyDescription string
}

var builtinDefinitions = []builtinDefinition{
	{
		name: "CPU pi calculation", artifact: "cpu-pi", architecture: "amd64",
		description:       "Runs the Leibniz series across 4 parallel AWK processes and checks the value of pi. Adjust the last two arg lines to change the process count and the iterations per process.",
		legacyName:        "CPU 圆周率计算",
		legacyDescription: "使用 4 个并行 AWK 计算进程执行 Leibniz 级数并校验圆周率结果；可修改最后两个 arg 调整进程数和每进程迭代次数。",
		body: `project = cpu-validation
queue = default
priority = 0
attempts = 1
cpu = 4
memory = 256M
timeout = 10m
arg = 4
arg = 2500000`,
	},
	{
		name: "CUDA smoke test", artifact: "cuda-smoke", architecture: "amd64",
		description:       "Claims one NVIDIA GPU, then compiles and runs a CUDA vector addition with nvcc to check device enumeration, memory transfer and the kernel result.",
		legacyName:        "CUDA 基础测试",
		legacyDescription: "申请 1 张 NVIDIA GPU，使用 nvcc 编译并运行 CUDA 向量加法，同时验证设备枚举、内存传输和核函数结果。",
		body: `project = cuda-validation
queue = default
priority = 10
attempts = 1
cpu = 2
memory = 1G
device.nvidia.com/gpu = 1
timeout = 10m
env.PATH = /usr/local/cuda/bin:/usr/bin:/bin`,
	},
	{
		name: "ARM64 node acceptance", artifact: "arm64-acceptance", architecture: "arm64",
		description:       "Schedules only onto ARM64 nodes and checks the architecture, kernel, CPU count, instruction set, memory, integer arithmetic and whether the job working directory is writable.",
		legacyName:        "ARM64 节点验收",
		legacyDescription: "仅调度到 ARM64 节点，检查架构、内核、CPU 数量、指令集、内存、整数计算和任务工作目录写入能力。",
		body: `project = arm64-validation
queue = default
priority = 5
attempts = 1
cpu = 2
memory = 512M
timeout = 5m`,
	},
}

// EnsureBuiltins publishes the versioned system templates once. The marker
// allows administrators to edit or remove them later without a restart
// silently recreating their changes.
//
// The legacy relabel runs ahead of the marker check, because a cluster that
// seeded the Chinese-named built-ins has that marker in place and would
// otherwise keep them forever.
func EnsureBuiltins(ctx context.Context, service *Service, markerPath, artifactRoot string) error {
	if service == nil || !filepath.IsAbs(markerPath) || !filepath.IsAbs(artifactRoot) {
		return ErrInvalid
	}
	if err := relabelLegacyBuiltins(ctx, service); err != nil {
		return err
	}
	if _, err := os.Stat(markerPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	existing, err := service.List(ctx)
	if err != nil {
		return err
	}
	names := make(map[string]bool, len(existing))
	for _, template := range existing {
		names[strings.ToLower(strings.TrimSpace(template.Name))] = true
	}
	actor := domain.Principal{Subject: "xirang-system", DisplayName: "XIRANG", Role: "admin", AuthMethod: "system"}
	for _, definition := range builtinDefinitions {
		if names[strings.ToLower(definition.name)] {
			continue
		}
		script, err := renderBuiltinScript(artifactRoot, definition)
		if err != nil {
			return err
		}
		if _, err := service.Create(ctx, definition.name, definition.description, script, actor); err != nil {
			return err
		}
	}
	return writeBuiltinMarker(markerPath)
}

// relabelLegacyBuiltins moves the built-in templates a previous release seeded
// under Simplified Chinese names onto their English ones, so that the console
// can localise them from a known key instead of showing whichever language the
// row happened to be written in.
//
// A row is only touched when both its name and its description still match what
// this package shipped. An administrator who renamed a template, or rewrote its
// description, has made it their content, and it is left exactly as it is. That
// makes the pass idempotent: once a row is renamed nothing matches it again, so
// it needs no schema marker of its own.
func relabelLegacyBuiltins(ctx context.Context, service *Service) error {
	existing, err := service.List(ctx)
	if err != nil {
		return err
	}
	legacy := make(map[string]builtinDefinition, len(builtinDefinitions))
	for _, definition := range builtinDefinitions {
		if definition.legacyName != "" {
			legacy[definition.legacyName] = definition
		}
	}
	taken := make(map[string]bool, len(existing))
	for _, template := range existing {
		taken[strings.ToLower(strings.TrimSpace(template.Name))] = true
	}
	actor := domain.Principal{Subject: "xirang-system", DisplayName: "XIRANG", Role: "admin", AuthMethod: "system"}
	for _, template := range existing {
		definition, ok := legacy[strings.TrimSpace(template.Name)]
		if !ok || strings.TrimSpace(template.Description) != definition.legacyDescription {
			continue
		}
		// Renaming onto a name an administrator already uses would collide, and
		// their template is the one that should keep it.
		if taken[strings.ToLower(definition.name)] {
			continue
		}
		if _, err := service.Update(ctx, template.ID, template.Revision, definition.name, definition.description, template.Script, actor); err != nil {
			return err
		}
		taken[strings.ToLower(definition.name)] = true
	}
	return nil
}

func renderBuiltinScript(root string, definition builtinDefinition) (string, error) {
	artifactPath := filepath.Join(root, "v1", definition.artifact)
	digest, err := sha256File(artifactPath)
	if err != nil {
		return "", fmt.Errorf("read built-in artifact %s: %w", definition.artifact, err)
	}
	artifactURL := (&url.URL{Scheme: "file", Path: artifactPath}).String()
	return fmt.Sprintf(`XIRANG 1
# Versioned system artifact available through the cluster shared path.
artifact = %s
sha256 = %s
architecture = %s
entrypoint = %s
%s
`, artifactURL, digest, definition.architecture, definition.artifact, definition.body), nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeBuiltinMarker(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".builtin-job-templates-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	// Both the chmod and the write must be assigned to the outer err. Declaring
	// a scoped one here discarded a failed write, so an empty marker could be
	// renamed into place and read back as a completed seeding.
	if err = file.Chmod(0o600); err == nil {
		_, err = file.WriteString("v1\n")
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
