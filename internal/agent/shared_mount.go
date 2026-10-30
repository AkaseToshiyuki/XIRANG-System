package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

type mountExecution func(context.Context, *xirangv1.SharedMountOperation, []string) error
type mountRecord struct {
	operation *xirangv1.SharedMountOperation
	result    *xirangv1.SharedMountResult
}
type sharedMountManager struct {
	ctx     context.Context
	run     mountExecution
	mu      sync.Mutex
	busy    bool
	records map[string]*mountRecord
	results chan *xirangv1.SharedMountResult
}

func newSharedMountManager(ctx context.Context, run mountExecution) *sharedMountManager {
	return &sharedMountManager{ctx: ctx, run: run, records: map[string]*mountRecord{}, results: make(chan *xirangv1.SharedMountResult, 16)}
}

func (manager *sharedMountManager) Start(operation *xirangv1.SharedMountOperation) (bool, string) {
	if manager == nil || manager.run == nil {
		return false, "shared mount control is disabled"
	}
	args, err := mountcontrol.Arguments(operation, time.Now())
	if err != nil {
		return false, err.Error()
	}
	if err := manager.ctx.Err(); err != nil {
		return false, "control stream disconnected"
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	// A completed record cannot authorize a new replay once its original
	// deadline has expired. Retire it so long-lived sessions remain usable.
	for id, record := range manager.records {
		if record.result != nil && !record.operation.ExpiresAt.AsTime().After(time.Now()) {
			delete(manager.records, id)
		}
	}
	if recorded := manager.records[operation.OperationId]; recorded != nil {
		if !proto.Equal(recorded.operation, operation) {
			return false, "operation ID reused with different parameters"
		}
		if recorded.result != nil {
			select {
			case manager.results <- recorded.result:
			default:
			}
		}
		return true, ""
	}
	if manager.busy {
		return false, "another shared mount operation is running"
	}
	if len(manager.records) >= 1024 {
		return false, "shared mount session operation limit reached; wait for outstanding deadlines"
	}
	record := &mountRecord{operation: proto.Clone(operation).(*xirangv1.SharedMountOperation)}
	manager.records[operation.OperationId] = record
	manager.busy = true
	go func() {
		ctx, cancel := context.WithDeadline(manager.ctx, record.operation.ExpiresAt.AsTime())
		defer cancel()
		err := ctx.Err()
		if err == nil {
			err = manager.run(ctx, record.operation, args)
		}
		if err == nil {
			err = ctx.Err()
		}
		result := &xirangv1.SharedMountResult{OperationId: record.operation.OperationId, Success: err == nil}
		if err != nil {
			message := strings.ToValidUTF8(err.Error(), "?")
			if len(message) > 1000 {
				message = strings.ToValidUTF8(message[:1000], "?")
			}
			result.Message = message
		}
		manager.mu.Lock()
		record.result = result
		manager.busy = false
		manager.mu.Unlock()
		select {
		case manager.results <- result:
		case <-manager.ctx.Done():
		}
	}()
	return true, ""
}

// systemd must create this service in the host namespace: the Agent's
// ProtectSystem=strict mount namespace cannot perform host mount mutations.
// The helper's host lock rejects overlap, including across Agent restarts. Unit
// runtime and stop deadlines still apply if the Agent process dies.
func configuredMountExecution() (mountExecution, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("shared mount control requires a root systemd Agent")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return nil, errors.New("shared mount control requires systemd")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(executable)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("shared mount control requires a root-owned installed Agent binary")
	}
	for parent := filepath.Dir(executable); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
			return nil, errors.New("agent installation directory must be root-owned and not writable by other users")
		}
		if parent == "/" {
			break
		}
	}
	for _, path := range []string{"/usr/bin/systemd-run", "/usr/bin/systemctl"} {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context, operation *xirangv1.SharedMountOperation, args []string) error {
		command := exec.CommandContext(ctx, "/usr/bin/systemd-run", sharedMountUnitArguments(executable, operation, args)...)
		var output mountOutput
		command.Stdout, command.Stderr = &output, &output
		command.WaitDelay = 3 * time.Second
		err := command.Run()
		if ctx.Err() != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Stop only this operation's unit, never a subsequent/replacement Agent's
			// work. RuntimeMaxSec also bounds a lost or delayed stop request.
			stop := exec.CommandContext(stopCtx, "/usr/bin/systemctl", "stop", "xirang-shared-"+operation.OperationId+".service")
			stop.WaitDelay = time.Second
			if stopErr := stop.Run(); stopErr != nil {
				return fmt.Errorf("mount interrupted; host unit stop unconfirmed: %w", ctx.Err())
			}
			return fmt.Errorf("mount interrupted; effects may be partial: %w", ctx.Err())
		}
		if err != nil {
			return fmt.Errorf("host mount unit failed: %w: %s", err, output.String())
		}
		return nil
	}, nil
}

func sharedMountUnitArguments(executable string, operation *xirangv1.SharedMountOperation, args []string) []string {
	result := []string{"--unit=xirang-shared-" + operation.OperationId, "--quiet", "--wait", "--pipe", "--collect", "--service-type=exec",
		"--property=RuntimeMaxSec=30s", "--property=TimeoutStartSec=5s", "--property=TimeoutStopSec=2s", "--property=KillMode=control-group",
		"--property=User=root", "--property=UMask=0077", "--property=NoNewPrivileges=yes", "--property=WorkingDirectory=/",
		"--setenv=PATH=/usr/sbin:/usr/bin:/sbin:/bin", "--description=XIRANG shared mount " + operation.OperationId,
		"--", executable, "__xirang-shared-mount", strconv.FormatInt(operation.ExpiresAt.AsTime().UnixNano(), 10)}
	return append(result, args...)
}

type mountOutput struct {
	mu   sync.Mutex
	data []byte
}

func (output *mountOutput) Write(p []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	n := len(p)
	if n > 1024 {
		p = p[n-1024:]
	}
	if extra := len(output.data) + len(p) - 1024; extra > 0 {
		output.data = output.data[extra:]
	}
	output.data = append(output.data, p...)
	return n, nil
}
func (output *mountOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return strings.TrimSpace(string(output.data))
}

// RunHostSharedMount rechecks expiry at the actual host service entry, after
// systemd scheduling. This internal entry point is not a general command runner.
func RunHostSharedMount(args []string) error {
	if os.Geteuid() != 0 || len(args) < 4 {
		return errors.New("invalid host mount invocation")
	}
	expires, err := strconv.ParseInt(args[0], 10, 64)
	now := time.Now()
	if err != nil || !time.Unix(0, expires).After(now) || time.Unix(0, expires).After(now.Add(mountcontrol.Timeout)) {
		return errors.New("host mount operation expired or invalid")
	}
	switch args[1] {
	case "prepare", "export", "unexport", "mount", "unmount":
	default:
		return errors.New("unsupported host mount operation")
	}
	lock, err := unix.Open("/run/xirang-shared-mount.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer unix.Close(lock)
	var stat unix.Stat_t
	if err := unix.Fstat(lock, &stat); err != nil {
		return err
	}
	if stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Mode&0o077 != 0 {
		return errors.New("invalid host mount lock")
	}
	if err := unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("another host mount operation is running")
	}
	return RunMountHelper(args[1:])
}
