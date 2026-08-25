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
}

var builtinDefinitions = []builtinDefinition{
	{
		name: "CPU 圆周率计算", artifact: "cpu-pi", architecture: "amd64",
		description: "使用 4 个并行 AWK 计算进程执行 Leibniz 级数并校验圆周率结果；可修改最后两个 arg 调整进程数和每进程迭代次数。",
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
		name: "CUDA 基础测试", artifact: "cuda-smoke", architecture: "amd64",
		description: "申请 1 张 NVIDIA GPU，使用 nvcc 编译并运行 CUDA 向量加法，同时验证设备枚举、内存传输和核函数结果。",
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
		name: "ARM64 节点验收", artifact: "arm64-acceptance", architecture: "arm64",
		description: "仅调度到 ARM64 节点，检查架构、内核、CPU 数量、指令集、内存、整数计算和任务工作目录写入能力。",
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
func EnsureBuiltins(ctx context.Context, service *Service, markerPath, artifactRoot string) error {
	if service == nil || !filepath.IsAbs(markerPath) || !filepath.IsAbs(artifactRoot) {
		return ErrInvalid
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
	if err := file.Chmod(0o600); err == nil {
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
