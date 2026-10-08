//go:build linux

package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/safefile"
	"golang.org/x/sys/unix"
)

type NativeRunnerConfig struct {
	ID         string
	User       string
	Group      string
	CgroupRoot string
	Toolchains []string
	// SharedWorkspace keeps ownership unchanged for an administrator-managed
	// setgid filesystem such as an all_squash NFS export.
	SharedWorkspace bool
}

type NativeRunner struct {
	worker     domain.BuildWorker
	credential *syscall.Credential
	cgroupRoot string
	shared     bool
	lockRoot   string
	leaseMu    sync.Mutex
	lease      *os.File
	quarantine error
}

func NewNativeRunner(config NativeRunnerConfig) (*NativeRunner, error) {
	if config.ID == "" {
		config.ID = "local-" + runtime.GOARCH
	}
	if config.User == "" || config.CgroupRoot == "" || !filepath.IsAbs(config.CgroupRoot) {
		return nil, errors.New("native build runner requires a dedicated user and absolute delegated cgroup root")
	}
	if err := buildFilesystemABI(); err != nil {
		return nil, err
	}
	account, err := user.Lookup(config.User)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid == 0 || int(uid) == os.Geteuid() {
		return nil, errors.New("native build user must be a non-root numeric identity")
	}
	gidText := account.Gid
	if config.Group != "" {
		group, err := user.LookupGroup(config.Group)
		if err != nil {
			return nil, err
		}
		gidText = group.Gid
	}
	gid, err := strconv.ParseUint(gidText, 10, 32)
	if err != nil || gid == 0 {
		return nil, errors.New("native build group must be non-root")
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return nil, err
	}
	supplementary := make([]uint32, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		parsed, parseErr := strconv.ParseUint(groupID, 10, 32)
		if parseErr != nil || parsed == 0 {
			return nil, errors.New("native build user has an invalid supplementary group")
		}
		if uint32(parsed) != uint32(gid) {
			supplementary = append(supplementary, uint32(parsed))
		}
	}
	if os.Geteuid() != 0 && !hasBuildIdentityCapabilities() {
		return nil, errors.New("build runner requires CAP_CHOWN, CAP_SETGID, and CAP_SETUID to isolate a dedicated build identity")
	}
	if err := os.MkdirAll(config.CgroupRoot, 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(config.CgroupRoot, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("build cgroup root is not delegated cgroup v2: %w", err)
	}
	if err := os.WriteFile(filepath.Join(config.CgroupRoot, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644); err != nil {
		return nil, fmt.Errorf("enable build cgroup controllers: %w", err)
	}
	if len(config.Toolchains) == 0 {
		for _, toolchain := range []string{"go", "cc", "make"} {
			if _, err := exec.LookPath(toolchain); err == nil {
				config.Toolchains = append(config.Toolchains, toolchain)
			}
		}
	}
	if len(config.Toolchains) == 0 {
		return nil, errors.New("native build runner found no supported toolchain in PATH")
	}
	capabilities := discoverToolchainCapabilities(config.Toolchains)
	if len(capabilities) == 0 {
		return nil, errors.New("native build runner found no configured toolchain executable in PATH")
	}
	availableToolchains := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		availableToolchains = append(availableToolchains, capability.Name)
	}
	runner := &NativeRunner{
		worker: domain.BuildWorker{
			ID: config.ID, Architecture: runtime.GOARCH, Toolchains: availableToolchains,
			ToolchainCapabilities: capabilities,
			Isolation:             "dedicated-uid+cgroup-v2+landlock+no-new-privileges", Ready: true,
			SupportsCancellation: true,
		},
		credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: supplementary},
		cgroupRoot: config.CgroupRoot,
		shared:     config.SharedWorkspace,
	}
	lock, err := runner.openIdentityLock()
	if err != nil {
		return nil, err
	}
	lock.Close()
	return runner, nil
}

func hasBuildIdentityCapabilities() bool {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return false
	}
	for _, capability := range []uint{unix.CAP_CHOWN, unix.CAP_SETGID, unix.CAP_SETUID} {
		index, bit := capability/32, capability%32
		if data[index].Effective&(uint32(1)<<bit) == 0 {
			return false
		}
	}
	return true
}

func (runner *NativeRunner) Worker() domain.BuildWorker { return runner.worker }

func (runner *NativeRunner) Run(ctx context.Context, workspace string, recipe domain.BuildRecipe) (logsResult string, runError error) {
	outputPath := filepath.Join(workspace, filepath.FromSlash(recipe.OutputPath))
	outputDirectoryMode := os.FileMode(0o711)
	if runner.shared {
		outputDirectoryMode = 0o2770
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), outputDirectoryMode); err != nil {
		return "", err
	}
	if runner.shared {
		if err := os.Chmod(filepath.Dir(outputPath), outputDirectoryMode); err != nil {
			return "", err
		}
	}
	var paths []string
	if err := filepath.Walk(workspace, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return "", err
	}
	// Walk is pre-order. Changing the root first can revoke the controller's
	// traversal permission before it reaches children, so transfer ownership
	// from leaves to root.
	for index := len(paths) - 1; index >= 0 && !runner.shared; index-- {
		if info, err := os.Stat(paths[index]); err != nil {
			return "", err
		} else if info.IsDir() {
			// The controller retains only search access while the unprivileged
			// build identity owns the directory. This lets it reclaim the
			// checksum-pinned output without exposing directory listings.
			if err := os.Chmod(paths[index], 0o711); err != nil {
				return "", err
			}
		}
		if err := os.Chown(paths[index], int(runner.credential.Uid), int(runner.credential.Gid)); err != nil {
			return "", err
		}
	}
	group := filepath.Join(runner.cgroupRoot, filepath.Base(workspace))
	if err := os.Mkdir(group, 0o755); err != nil {
		return "", err
	}
	defer func() {
		if err := stopBuildCgroup(group); err != nil {
			runner.quarantineIdentity(err)
			runError = errors.Join(runError, err)
		} else {
			cleanupBuildCgroup(group)
		}
	}()
	quota := recipe.MilliCPU * 100
	for name, value := range map[string]string{
		"cpu.max":    fmt.Sprintf("%d 100000", quota),
		"memory.max": strconv.FormatInt(recipe.MemoryBytes, 10),
		"pids.max":   "1024",
	} {
		if err := os.WriteFile(filepath.Join(group, name), []byte(value), 0o644); err != nil {
			return "", fmt.Errorf("configure build cgroup %s: %w", name, err)
		}
	}
	output := newBuildLogTail(64 << 10)
	for index, step := range buildSteps(recipe) {
		_, _ = fmt.Fprintf(output, "[step %d/%d] %s\n", index+1, len(buildSteps(recipe)), strings.Join(step, " "))
		if err := runner.runBuildStep(ctx, workspace, group, step, recipe.Environment, output); err != nil {
			return output.String(), err
		}
	}
	logs := output.String()
	if err := stopBuildCgroup(group); err != nil {
		return logs, err
	}
	if err := runner.reclaimOutput(workspace, recipe.OutputPath); err != nil {
		return logs, err
	}
	return logs, nil
}

func (runner *NativeRunner) reclaimOutput(workspace, relative string) error {
	output, err := safefile.OpenRegularForMetadata(workspace, relative)
	if err != nil {
		return fmt.Errorf("open declared build output: %w", err)
	}
	defer output.Close()
	if runner.shared {
		// /proc/self/fd refers to the pinned O_PATH descriptor, including
		// after a rename. This also works before fchmodat2 support.
		if err := os.Chmod(fmt.Sprintf("/proc/self/fd/%d", output.Fd()), 0o550); err != nil {
			return fmt.Errorf("protect shared build output: %w", err)
		}
	} else if err := unix.Fchownat(int(output.Fd()), "", os.Geteuid(), os.Getegid(), unix.AT_EMPTY_PATH); err != nil {
		return fmt.Errorf("reclaim build output: %w", err)
	}
	return nil
}

// A successful parent process can leave descendants behind. Do not inspect or
// reclaim files until the entire build cgroup has become empty.
func stopBuildCgroup(path string) error {
	deadline := time.Now().Add(2 * time.Second)
	killRequested := false
	for {
		events, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(events), "\n") {
			if line == "populated 0" {
				return nil
			}
		}
		if !killRequested {
			if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644); err != nil {
				return err
			}
			killRequested = true
		}
		if time.Now().After(deadline) {
			return errors.New("build processes did not exit before output collection")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (runner *NativeRunner) runBuildStep(ctx context.Context, workspace, group string, step []string, environment map[string]string, output io.Writer) error {
	executable, err := exec.LookPath(step[0])
	if err != nil {
		return err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer writer.Close()
	command := exec.CommandContext(ctx, "/proc/self/exe", append([]string{"__xirang-build-exec", executable}, step[1:]...)...)
	command.Dir = workspace
	command.ExtraFiles = []*os.File{reader}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL, Credential: runner.credential}
	command.Cancel = func() error { return killBuildProcessTree(command, group) }
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + workspace, "LANG=C.UTF-8", "SOURCE_DATE_EPOCH=0"}
	if runner.shared {
		command.Env = append(command.Env, "XIRANG_BUILD_SHARED_WORKSPACE=1")
	}
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		_ = reader.Close()
		return err
	}
	_ = reader.Close()
	if err := os.WriteFile(filepath.Join(group, "cgroup.procs"), []byte(strconv.Itoa(command.Process.Pid)), 0o644); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return err
	}
	if _, err := writer.Write([]byte{1}); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return err
	}
	_ = writer.Close()
	return command.Wait()
}

func killBuildProcessTree(command *exec.Cmd, cgroup string) error {
	// cgroup.kill is the primary path because the controller deliberately does
	// not retain CAP_KILL after launching the different-UID build account.
	cgroupErr := os.WriteFile(filepath.Join(cgroup, "cgroup.kill"), []byte("1"), 0o644)
	groupErr := killBuildProcessGroup(command)
	if cgroupErr == nil || groupErr == nil {
		return nil
	}
	if errors.Is(groupErr, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return errors.Join(cgroupErr, groupErr)
}

func killBuildProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func RunExecutorLauncher(arguments []string) error {
	// Landlock and no_new_privs apply to the calling OS thread. Keep exec on it.
	runtime.LockOSThread()
	if len(arguments) == 0 || !filepath.IsAbs(arguments[0]) {
		return errors.New("build launcher requires an absolute executable")
	}
	gate := os.NewFile(3, "xirang-build-gate")
	if gate == nil {
		return errors.New("build admission gate unavailable")
	}
	defer gate.Close()
	buffer := []byte{0}
	if count, err := gate.Read(buffer); err != nil || count != 1 || buffer[0] != 1 {
		return errors.New("build process was not admitted to cgroup")
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	if os.Getenv("XIRANG_BUILD_SHARED_WORKSPACE") == "1" {
		syscall.Umask(0o007)
	} else {
		syscall.Umask(0o077)
	}
	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := restrictBuildFilesystem(workspace); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".tmp"), 0700); err != nil {
		return err
	}
	if err := os.Setenv("TMPDIR", filepath.Join(workspace, ".tmp")); err != nil {
		return err
	}
	return syscall.Exec(arguments[0], append([]string{arguments[0]}, arguments[1:]...), os.Environ())
}

func cleanupBuildCgroup(path string) {
	_ = os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o644)
	_ = os.Remove(path)
}
