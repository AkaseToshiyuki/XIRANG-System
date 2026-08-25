package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const remoteNativeBuildCapability = "remote-native-build-v1"

const (
	maxNativeBuildFiles     = 100_000
	maxNativeBuildFileBytes = int64(1 << 30)
	maxNativeBuildTreeBytes = int64(8 << 30)
)

type NativeBuildConfig struct {
	Enabled            bool
	WorkspaceRoot      string
	LocalWorkspaceRoot string
	User               string
	Group              string
	CgroupRoot         string
	Toolchains         []string
	MaxConcurrent      int
}

type NativeBuildManager struct {
	runner             *buildmanager.NativeRunner
	workspaceRoot      string
	localWorkspaceRoot string
	semaphore          chan struct{}
	events             chan *xirangv1.NativeBuildUpdate
	mu                 sync.Mutex
	running            map[string]bool
	cancel             map[string]context.CancelFunc
}

func OpenNativeBuildManager(config NativeBuildConfig) (*NativeBuildManager, error) {
	if !config.Enabled {
		return nil, nil
	}
	if config.WorkspaceRoot == "" || !filepath.IsAbs(config.WorkspaceRoot) || filepath.Clean(config.WorkspaceRoot) != config.WorkspaceRoot {
		return nil, errors.New("native build workspace root must be an absolute clean path")
	}
	if config.LocalWorkspaceRoot == "" || !filepath.IsAbs(config.LocalWorkspaceRoot) || filepath.Clean(config.LocalWorkspaceRoot) != config.LocalWorkspaceRoot {
		return nil, errors.New("native build local workspace root must be an absolute clean path")
	}
	if err := os.MkdirAll(config.LocalWorkspaceRoot, 0o711); err != nil {
		return nil, err
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 1
	}
	// Native builds currently use one dedicated operating-system identity.
	// Serializing them prevents workspaces owned by that identity from being
	// visible to a second, concurrently executing build.
	if config.MaxConcurrent != 1 {
		return nil, errors.New("native build concurrency must be 1 until per-build identities are enabled")
	}
	resolvedRoot, err := filepath.EvalSymlinks(config.WorkspaceRoot)
	if err != nil {
		return nil, errors.New("native build workspace root is unavailable")
	}
	runner, err := buildmanager.NewNativeRunner(buildmanager.NativeRunnerConfig{
		ID: "agent-local-" + runtime.GOARCH, User: config.User, Group: config.Group,
		CgroupRoot: config.CgroupRoot, Toolchains: config.Toolchains,
	})
	if err != nil {
		return nil, err
	}
	return &NativeBuildManager{
		runner: runner, workspaceRoot: resolvedRoot, localWorkspaceRoot: config.LocalWorkspaceRoot,
		semaphore: make(chan struct{}, config.MaxConcurrent),
		events:    make(chan *xirangv1.NativeBuildUpdate, config.MaxConcurrent*4), running: make(map[string]bool), cancel: make(map[string]context.CancelFunc),
	}, nil
}

func (manager *NativeBuildManager) WorkerReport() *xirangv1.NativeBuildWorker {
	if manager == nil {
		return nil
	}
	worker := manager.runner.Worker()
	return &xirangv1.NativeBuildWorker{
		Architecture: worker.Architecture, Toolchains: append([]string(nil), worker.Toolchains...),
		Isolation: worker.Isolation, Ready: worker.Ready,
		ToolchainCapabilities: nativeBuildCapabilityReport(worker.ToolchainCapabilities),
		SupportsCancellation:  true,
	}
}

func nativeBuildCapabilityReport(items []domain.ToolchainCapability) []*xirangv1.ToolchainCapability {
	result := make([]*xirangv1.ToolchainCapability, 0, len(items))
	for _, item := range items {
		result = append(result, &xirangv1.ToolchainCapability{Name: item.Name, Version: item.Version, TargetArchitectures: append([]string(nil), item.TargetArchitectures...), Executables: append([]string(nil), item.Executables...)})
	}
	return result
}

func (manager *NativeBuildManager) Events() <-chan *xirangv1.NativeBuildUpdate {
	if manager == nil {
		return nil
	}
	return manager.events
}

func (manager *NativeBuildManager) Start(command *xirangv1.RunNativeBuild) (bool, string) {
	if manager == nil {
		return false, "remote native build worker is disabled"
	}
	recipe, workspace, err := manager.validate(command)
	if err != nil {
		return false, err.Error()
	}
	manager.mu.Lock()
	if manager.running[command.BuildId] {
		manager.mu.Unlock()
		return true, ""
	}
	select {
	case manager.semaphore <- struct{}{}:
		ctx, cancel := context.WithCancel(context.Background())
		manager.running[command.BuildId] = true
		manager.cancel[command.BuildId] = cancel
		manager.mu.Unlock()
		go manager.run(ctx, cancel, command.BuildId, workspace, recipe)
	default:
		manager.mu.Unlock()
		return false, "native build concurrency limit reached"
	}
	return true, ""
}

func (manager *NativeBuildManager) run(ctx context.Context, cancel context.CancelFunc, buildID, workspace string, recipe domain.BuildRecipe) {
	defer func() {
		cancel()
		<-manager.semaphore
		manager.mu.Lock()
		delete(manager.running, buildID)
		delete(manager.cancel, buildID)
		manager.mu.Unlock()
	}()
	manager.events <- &xirangv1.NativeBuildUpdate{
		BuildId: buildID, State: xirangv1.NativeBuildState_NATIVE_BUILD_STATE_RUNNING, OccurredAt: timestamppb.Now(),
	}
	localWorkspace := filepath.Join(manager.localWorkspaceRoot, buildID)
	_ = os.RemoveAll(localWorkspace)
	err := copyNativeBuildTree(workspace, localWorkspace)
	if err == nil {
		var digest string
		digest, err = hashNativeBuildTree(localWorkspace)
		if err == nil && !strings.EqualFold(digest, recipe.SourceSHA256) {
			err = errors.New("shared build source changed before private staging")
		}
	}
	logs := ""
	outputDigest := ""
	if err == nil {
		runCtx, timeoutCancel := context.WithTimeout(ctx, recipe.Timeout)
		logs, err = manager.runner.Run(runCtx, localWorkspace, recipe)
		timeoutCancel()
	}
	if err == nil {
		outputDigest, err = publishNativeBuildOutput(
			filepath.Join(localWorkspace, filepath.FromSlash(recipe.OutputPath)),
			workspace, recipe.OutputPath,
		)
	}
	_ = os.RemoveAll(localWorkspace)
	update := &xirangv1.NativeBuildUpdate{
		BuildId: buildID, LogTail: boundedNativeBuildText(logs, 64<<10), OccurredAt: timestamppb.Now(),
		State: xirangv1.NativeBuildState_NATIVE_BUILD_STATE_SUCCEEDED, OutputSha256: outputDigest,
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			update.State = xirangv1.NativeBuildState_NATIVE_BUILD_STATE_CANCELED
			update.Failure = "native build canceled by controller"
		} else {
			update.State = xirangv1.NativeBuildState_NATIVE_BUILD_STATE_FAILED
			update.Failure = boundedNativeBuildText(err.Error(), 4096)
		}
	}
	manager.events <- update
}

func (manager *NativeBuildManager) Cancel(command *xirangv1.CancelNativeBuild) (bool, string) {
	if manager == nil || command == nil || command.BuildId == "" {
		return false, "native build cancellation is invalid"
	}
	manager.mu.Lock()
	cancel := manager.cancel[command.BuildId]
	running := manager.running[command.BuildId]
	manager.mu.Unlock()
	if !running {
		return true, ""
	}
	if cancel == nil {
		return false, "native build is still entering its execution context"
	}
	cancel()
	return true, ""
}

func (manager *NativeBuildManager) validate(command *xirangv1.RunNativeBuild) (domain.BuildRecipe, string, error) {
	if command == nil || command.BuildId == "" || filepath.Base(command.BuildId) != command.BuildId ||
		command.Architecture != manager.runner.Worker().Architecture || command.Timeout == nil ||
		command.Timeout.AsDuration() <= 0 || command.Timeout.AsDuration() > 2*time.Hour ||
		command.MilliCpu < 10 || command.MemoryBytes <= 0 || (len(command.Command) == 0 && len(command.Steps) == 0) || len(command.Steps) > 16 || len(command.Command) > 64 ||
		len(command.Environment) > 128 || !nativeBuildSafeRelative(command.OutputPath) || !nativeBuildSHA256(command.WorkspaceSha256) {
		return domain.BuildRecipe{}, "", errors.New("invalid remote native build command")
	}
	if !containsString(manager.runner.Worker().Toolchains, command.Toolchain) {
		return domain.BuildRecipe{}, "", errors.New("requested build toolchain is unavailable")
	}
	if !filepath.IsAbs(command.Workspace) || filepath.Clean(command.Workspace) != command.Workspace {
		return domain.BuildRecipe{}, "", errors.New("remote build workspace must be an absolute clean path")
	}
	workspace, err := filepath.EvalSymlinks(command.Workspace)
	if err != nil {
		return domain.BuildRecipe{}, "", errors.New("remote build workspace is unavailable")
	}
	relative, err := filepath.Rel(manager.workspaceRoot, workspace)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.Base(workspace) != command.BuildId {
		return domain.BuildRecipe{}, "", errors.New("remote build workspace escapes the configured shared root")
	}
	for _, argument := range command.Command {
		if argument == "" || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return domain.BuildRecipe{}, "", errors.New("remote build command argument is invalid")
		}
	}
	steps := make([][]string, 0, len(command.Steps))
	for _, step := range command.Steps {
		if step == nil || len(step.Arguments) == 0 || len(step.Arguments) > 64 {
			return domain.BuildRecipe{}, "", errors.New("remote native build step is invalid")
		}
		for _, argument := range step.Arguments {
			if argument == "" || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
				return domain.BuildRecipe{}, "", errors.New("remote native build step argument is invalid")
			}
		}
		steps = append(steps, append([]string(nil), step.Arguments...))
	}
	for name, value := range command.Environment {
		if name == "" || len(name) > 128 || strings.ContainsAny(name, "=\x00") || len(value) > 8192 ||
			strings.ContainsRune(value, '\x00') || strings.HasPrefix(name, "XIRANG_") || name == "PATH" || name == "HOME" || name == "LANG" || name == "SOURCE_DATE_EPOCH" {
			return domain.BuildRecipe{}, "", errors.New("remote build environment is invalid")
		}
	}
	return domain.BuildRecipe{
		Architecture: command.Architecture, Command: append([]string(nil), command.Command...), Steps: steps,
		OutputPath: command.OutputPath, Toolchain: command.Toolchain, Environment: command.Environment,
		MilliCPU: command.MilliCpu, MemoryBytes: command.MemoryBytes, Timeout: command.Timeout.AsDuration(),
		SourceSHA256: command.WorkspaceSha256,
	}, workspace, nil
}

func copyNativeBuildTree(source, destination string) error {
	var files int
	var bytes int64
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("shared build workspace contains a symbolic link")
		}
		relative, _ := filepath.Rel(source, path)
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !entry.Type().IsRegular() {
			return errors.New("shared build workspace contains an unsupported object")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		if files > maxNativeBuildFiles || info.Size() < 0 || info.Size() > maxNativeBuildFileBytes || bytes > maxNativeBuildTreeBytes {
			return errors.New("shared build workspace exceeds the staging file or byte limit")
		}
		if err := copyNativeBuildFile(path, target, info.Mode().Perm()&0o755, info.Size()); err != nil {
			return fmt.Errorf("stage private build file %s: %w", relative, err)
		}
		return nil
	})
}

func copyNativeBuildFile(source, destination string, mode os.FileMode, expectedBytes int64) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, expectedBytes+1))
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != expectedBytes {
		return errors.New("shared build source changed while it was staged")
	}
	return nil
}

func hashNativeBuildTree(root string) (string, error) {
	hash := sha256.New()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("native build tree contains a symbolic link")
		}
		if entry.Type().IsRegular() {
			relative, _ := filepath.Rel(root, path)
			paths = append(paths, relative)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	var totalBytes int64
	for _, relative := range paths {
		_, _ = hash.Write([]byte(filepath.ToSlash(relative) + "\x00"))
		path := filepath.Join(root, relative)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxNativeBuildFileBytes {
			return "", errors.New("native build tree contains an invalid or oversized file")
		}
		totalBytes += info.Size()
		if totalBytes > maxNativeBuildTreeBytes {
			return "", errors.New("native build tree exceeds the byte limit")
		}
		input, err := os.Open(path)
		if err != nil {
			return "", err
		}
		written, copyErr := io.Copy(hash, io.LimitReader(input, info.Size()+1))
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil || written != info.Size() {
			return "", errors.New("native build tree changed while it was hashed")
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func publishNativeBuildOutput(source, workspace, outputPath string) (string, error) {
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<30 {
		return "", errors.New("remote build output is missing, not regular, or exceeds 1 GiB")
	}
	if !nativeBuildSafeRelative(outputPath) {
		return "", errors.New("remote build output path is invalid")
	}
	directory, err := ensureNativeBuildDirectory(workspace, filepath.Dir(filepath.FromSlash(outputPath)))
	if err != nil {
		return "", err
	}
	destination := filepath.Join(workspace, filepath.FromSlash(outputPath))
	temporary, err := os.CreateTemp(directory, ".xirang-output-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o550); err != nil {
		_ = temporary.Close()
		return "", err
	}
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		_ = temporary.Close()
		return "", err
	}
	if stat, ok := workspaceInfo.Sys().(*syscall.Stat_t); ok {
		if err := temporary.Chown(-1, int(stat.Gid)); err != nil {
			_ = temporary.Close()
			return "", err
		}
	}
	input, err := os.Open(source)
	if err != nil {
		_ = temporary.Close()
		return "", err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(input, info.Size()+1))
	inputCloseErr := input.Close()
	if copyErr != nil || inputCloseErr != nil || written != info.Size() {
		_ = temporary.Close()
		return "", errors.New("remote build output changed while it was published")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ensureNativeBuildDirectory(root, relative string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	current := resolvedRoot
	if relative == "." {
		return current, nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("remote build output directory is invalid")
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o2770); err != nil {
				return "", err
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("remote build output parent is not a real directory")
		}
		if err := os.Chmod(current, 0o2770); err != nil {
			return "", err
		}
	}
	return current, nil
}

func nativeBuildSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func nativeBuildSafeRelative(value string) bool {
	if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != value || value == "." || value == ".." {
		return false
	}
	return !strings.HasPrefix(value, ".."+string(filepath.Separator))
}

func boundedNativeBuildText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
