package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	assignmentStateFile     = "assignment-state.json"
	maxArtifactBytes        = 1 << 30
	maxAssignmentLedger     = 4096
	maxAssignmentUpdates    = maxAssignmentLedger * 4
	maxAssignmentLogBytes   = 64 << 10
	assignmentLogInterval   = 500 * time.Millisecond
	assignmentUsageInterval = 2 * time.Second
	maxAssignmentLogUpdates = 256
)

type assignmentLedger struct {
	Assignments []assignmentRecord `json:"assignments"`
}

type assignmentRecord struct {
	AssignmentID  string                   `json:"assignmentId"`
	Attempt       uint32                   `json:"attempt"`
	SpecHash      string                   `json:"specHash"`
	State         xirangv1.AssignmentState `json:"state"`
	ExitCode      int32                    `json:"exitCode,omitempty"`
	Reason        string                   `json:"reason,omitempty"`
	StdoutTail    []byte                   `json:"stdoutTail,omitempty"`
	StderrTail    []byte                   `json:"stderrTail,omitempty"`
	LogsTruncated bool                     `json:"logsTruncated,omitempty"`
	OccurredAt    time.Time                `json:"occurredAt"`
}

type runningAssignment struct {
	cancel       chan time.Duration
	release      chan struct{}
	allocationID string
	credential   *syscall.Credential
	workspace    string
	mu           sync.RWMutex
	policy       assignmentStoragePolicy
	released     bool
}

type assignmentStoragePolicy struct {
	quotaBytes   uint64
	minFreeBytes uint64
}

type tailBuffer struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (buffer *tailBuffer) Write(input []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	inputLength := len(input)
	if inputLength >= maxAssignmentLogBytes {
		buffer.data = append(buffer.data[:0], input[inputLength-maxAssignmentLogBytes:]...)
		buffer.truncated = true
		return inputLength, nil
	}
	if overflow := len(buffer.data) + inputLength - maxAssignmentLogBytes; overflow > 0 {
		copy(buffer.data, buffer.data[overflow:])
		buffer.data = buffer.data[:len(buffer.data)-overflow]
		buffer.truncated = true
	}
	buffer.data = append(buffer.data, input...)
	return inputLength, nil
}

func (buffer *tailBuffer) snapshot() ([]byte, bool) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return append([]byte(nil), buffer.data...), buffer.truncated
}

// NativeExecutor runs verified artifacts directly on Linux and persists every
// assignment transition before publishing it to the control stream.
type NativeExecutor struct {
	mu                 sync.Mutex
	directory          string
	workspaceDirectory string
	cacheDirectory     string
	ledger             assignmentLedger
	running            map[string]*runningAssignment
	updates            chan *xirangv1.AssignmentUpdate
	logs               chan *xirangv1.AssignmentLogSnapshot
	usage              chan *xirangv1.AssignmentUsageSnapshot
	allowRoot          bool
	cgroups            *CgroupManager
	credentialTemplate *syscall.Credential
	uidPool            *executionUIDPool
	deviceResources    []string
	artifactTrustKeys  []ed25519.PublicKey
	artifactMu         sync.RWMutex
	artifactController *url.URL
	artifactHTTPClient *http.Client
}

var artifactCacheMu sync.Mutex

func (executor *NativeExecutor) SetArtifactTrustKeys(keys []ed25519.PublicKey) {
	executor.artifactTrustKeys = make([]ed25519.PublicKey, len(keys))
	for index := range keys {
		executor.artifactTrustKeys[index] = append(ed25519.PublicKey(nil), keys[index]...)
	}
}

func (executor *NativeExecutor) SetArtifactHTTPClient(controllerURL string, client *http.Client) error {
	base, err := url.Parse(strings.TrimRight(controllerURL, "/"))
	if err != nil || base.Scheme != "https" || base.Host == "" || client == nil {
		return errors.New("authenticated artifact controller is invalid")
	}
	copyClient := *client
	copyClient.Timeout = 5 * time.Minute
	copyClient.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		if request.URL.Scheme != base.Scheme || !strings.EqualFold(request.URL.Host, base.Host) {
			return errors.New("controller artifact redirect changed origin")
		}
		return nil
	}
	executor.artifactMu.Lock()
	executor.artifactController = base
	executor.artifactHTTPClient = &copyClient
	executor.artifactMu.Unlock()
	return nil
}

type ExecutorIsolationConfig struct {
	CgroupRoot    string
	TaskUser      string
	TaskGroup     string
	TaskDirectory string
	TaskUIDMin    uint32
	TaskUIDMax    uint32
}

func OpenNativeExecutor(directory string) (*NativeExecutor, error) {
	return openNativeExecutor(directory)
}

func OpenIsolatedNativeExecutor(directory string, config ExecutorIsolationConfig) (*NativeExecutor, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("isolated native execution requires a root Agent")
	}
	if config.TaskUser == "" {
		return nil, errors.New("isolated native execution requires a dedicated task user")
	}
	if config.TaskDirectory == "" || !filepath.IsAbs(config.TaskDirectory) || filepath.Clean(config.TaskDirectory) != config.TaskDirectory {
		return nil, errors.New("isolated native execution requires an absolute clean task directory")
	}
	if config.TaskDirectory == string(filepath.Separator) || filepath.Clean(directory) == config.TaskDirectory {
		return nil, errors.New("task directory must be dedicated and separate from Agent state")
	}
	account, err := user.Lookup(config.TaskUser)
	if err != nil {
		return nil, fmt.Errorf("lookup task user %q: %w", config.TaskUser, err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid == 0 {
		return nil, errors.New("task user must resolve to a non-root numeric UID")
	}
	groupID := account.Gid
	if config.TaskGroup != "" {
		group, err := user.LookupGroup(config.TaskGroup)
		if err != nil {
			return nil, fmt.Errorf("lookup task group %q: %w", config.TaskGroup, err)
		}
		groupID = group.Gid
	}
	gid, err := strconv.ParseUint(groupID, 10, 32)
	if err != nil || gid == 0 {
		return nil, errors.New("task group must resolve to a non-root numeric GID")
	}
	if err := os.MkdirAll(config.TaskDirectory, 0o711); err != nil {
		return nil, fmt.Errorf("create isolated task directory: %w", err)
	}
	if err := os.Chmod(config.TaskDirectory, 0o711); err != nil {
		return nil, fmt.Errorf("protect isolated task directory: %w", err)
	}
	executor, err := openNativeExecutor(directory)
	if err != nil {
		return nil, err
	}
	manager, err := OpenCgroupManager(config.CgroupRoot)
	if err != nil {
		return nil, err
	}
	executor.allowRoot = true
	executor.cgroups = manager
	uidMinimum, uidMaximum, err := temporaryUIDRange(config.TaskUIDMin, config.TaskUIDMax)
	if err != nil {
		return nil, err
	}
	pool, err := newExecutionUIDPool(uidMinimum, uidMaximum, nil)
	if err != nil {
		return nil, err
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("list task user groups: %w", err)
	}
	groups := make([]uint32, 0, len(groupIDs))
	seenGroups := map[uint32]bool{uint32(gid): true}
	for _, text := range groupIDs {
		value, parseErr := strconv.ParseUint(text, 10, 32)
		if parseErr == nil && !seenGroups[uint32(value)] {
			seenGroups[uint32(value)] = true
			groups = append(groups, uint32(value))
		}
	}
	executor.credentialTemplate = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}
	executor.uidPool = pool
	probeUID, err := pool.Allocate("__xirang_device_probe__")
	if err != nil {
		return nil, err
	}
	probeCredential, err := executionCredential(executor.credentialTemplate, probeUID)
	pool.Release("__xirang_device_probe__")
	if err != nil {
		return nil, err
	}
	executor.deviceResources = manager.ConfigureDeviceAccess(probeCredential)
	executor.workspaceDirectory = config.TaskDirectory
	if err := sealStaleAssignmentWorkspaces(executor.workspaceDirectory); err != nil {
		return nil, fmt.Errorf("seal stale task workspaces: %w", err)
	}
	return executor, nil
}

func openNativeExecutor(directory string) (*NativeExecutor, error) {
	if directory == "" {
		return nil, errors.New("Agent state directory is required")
	}
	if err := os.MkdirAll(filepath.Join(directory, "assignments"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(directory, "cache", "artifacts"), 0o700); err != nil {
		return nil, err
	}
	executor := &NativeExecutor{
		directory:          directory,
		workspaceDirectory: filepath.Join(directory, "assignments"),
		cacheDirectory:     filepath.Join(directory, "cache", "artifacts"),
		running:            make(map[string]*runningAssignment),
		updates:            make(chan *xirangv1.AssignmentUpdate, maxAssignmentUpdates),
		logs:               make(chan *xirangv1.AssignmentLogSnapshot, maxAssignmentLogUpdates),
		usage:              make(chan *xirangv1.AssignmentUsageSnapshot, maxAssignmentLogUpdates),
	}
	data, err := os.ReadFile(filepath.Join(directory, assignmentStateFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &executor.ledger); err != nil {
			return nil, fmt.Errorf("decode Agent assignment state: %w", err)
		}
	}
	changed := false
	for index := range executor.ledger.Assignments {
		record := &executor.ledger.Assignments[index]
		if !assignmentTerminal(record.State) {
			record.State = xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED
			record.Reason = "Agent restarted before assignment completion"
			record.OccurredAt = time.Now().UTC()
			changed = true
		}
	}
	if changed {
		if err := executor.persistLocked(); err != nil {
			return nil, err
		}
	}
	return executor, nil
}

func (executor *NativeExecutor) AcceptRun(ctx context.Context, command *xirangv1.RunAssignment) (bool, string) {
	if err := executor.validateRunAssignment(command); err != nil {
		return false, err.Error()
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil {
		return false, err.Error()
	}
	specHash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	key := assignmentKey(command.AssignmentId, command.Attempt)
	executor.mu.Lock()
	for _, record := range executor.ledger.Assignments {
		if assignmentKey(record.AssignmentID, record.Attempt) != key {
			continue
		}
		executor.mu.Unlock()
		if record.SpecHash != specHash {
			return false, "assignment attempt was replayed with a different specification"
		}
		return true, ""
	}
	if len(executor.ledger.Assignments) >= maxAssignmentLedger {
		executor.mu.Unlock()
		return false, "assignment ledger capacity is exhausted; archive completed Agent state before accepting more work"
	}
	var credential *syscall.Credential
	if executor.uidPool != nil {
		uid, allocationErr := executor.uidPool.Allocate(key)
		if allocationErr != nil {
			executor.mu.Unlock()
			return false, allocationErr.Error()
		}
		credential, allocationErr = executionCredential(executor.credentialTemplate, uid)
		if allocationErr != nil {
			executor.uidPool.Release(key)
			executor.mu.Unlock()
			return false, allocationErr.Error()
		}
	}
	record := assignmentRecord{
		AssignmentID: command.AssignmentId,
		Attempt:      command.Attempt,
		SpecHash:     specHash,
		State:        xirangv1.AssignmentState_ASSIGNMENT_STATE_ACCEPTED,
		OccurredAt:   time.Now().UTC(),
	}
	executor.ledger.Assignments = append(executor.ledger.Assignments, record)
	running := &runningAssignment{
		cancel: make(chan time.Duration, 1), release: make(chan struct{}, 1), allocationID: command.AllocationId,
		credential: credential,
		policy:     assignmentStoragePolicy{quotaBytes: command.StorageQuotaBytes, minFreeBytes: command.StorageMinFreeBytes},
	}
	if !command.StartBarrier {
		running.release <- struct{}{}
		running.released = true
	}
	executor.running[key] = running
	if err := executor.persistLocked(); err != nil {
		executor.ledger.Assignments = executor.ledger.Assignments[:len(executor.ledger.Assignments)-1]
		delete(executor.running, key)
		if executor.uidPool != nil {
			executor.uidPool.Release(key)
		}
		executor.mu.Unlock()
		return false, fmt.Sprintf("persist assignment acceptance: %v", err)
	}
	executor.mu.Unlock()
	executor.publish(record.update())
	go executor.execute(ctx, command, running)
	return true, ""
}

func (executor *NativeExecutor) AcceptCancel(command *xirangv1.CancelAssignment) (bool, string) {
	if command == nil || strings.TrimSpace(command.AssignmentId) == "" {
		return false, "assignment ID is required"
	}
	grace := 5 * time.Second
	if command.GracePeriod != nil {
		grace = command.GracePeriod.AsDuration()
		if grace < 0 || grace > 5*time.Minute {
			return false, "cancel grace period must be between zero and five minutes"
		}
	}
	executor.mu.Lock()
	var targets []*runningAssignment
	var snapshots []*xirangv1.AssignmentUpdate
	found := false
	for _, record := range executor.ledger.Assignments {
		if record.AssignmentID != command.AssignmentId {
			continue
		}
		found = true
		if running := executor.running[assignmentKey(record.AssignmentID, record.Attempt)]; running != nil {
			targets = append(targets, running)
		} else {
			snapshots = append(snapshots, record.update())
		}
	}
	executor.mu.Unlock()
	if !found {
		return false, "assignment is unknown"
	}
	for _, target := range targets {
		select {
		case target.cancel <- grace:
		default:
		}
	}
	for _, update := range snapshots {
		executor.publish(update)
	}
	return true, ""
}

func (executor *NativeExecutor) AcceptRelease(command *xirangv1.ReleaseAssignment) (bool, string) {
	if command == nil || strings.TrimSpace(command.AssignmentId) == "" || strings.TrimSpace(command.AllocationId) == "" {
		return false, "assignment ID and allocation ID are required"
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for key, running := range executor.running {
		if !strings.HasPrefix(key, command.AssignmentId+"/") {
			continue
		}
		running.mu.Lock()
		if running.allocationID != command.AllocationId {
			running.mu.Unlock()
			return false, "allocation ID does not match the prepared assignment"
		}
		if !running.released {
			running.released = true
			running.release <- struct{}{}
		}
		running.mu.Unlock()
		return true, ""
	}
	return false, "assignment is not prepared"
}

func (executor *NativeExecutor) Updates() <-chan *xirangv1.AssignmentUpdate { return executor.updates }

func (executor *NativeExecutor) Logs() <-chan *xirangv1.AssignmentLogSnapshot { return executor.logs }

func (executor *NativeExecutor) Usage() <-chan *xirangv1.AssignmentUsageSnapshot {
	return executor.usage
}

func (executor *NativeExecutor) AcceptStoragePolicy(command *xirangv1.SetAssignmentStoragePolicy) (bool, string) {
	if command == nil || strings.TrimSpace(command.AssignmentId) == "" {
		return false, "assignment ID is required"
	}
	executor.mu.Lock()
	targets := make([]*runningAssignment, 0, 1)
	for key, running := range executor.running {
		if strings.HasPrefix(key, command.AssignmentId+"/") {
			targets = append(targets, running)
		}
	}
	executor.mu.Unlock()
	if len(targets) == 0 {
		return false, "assignment is not running"
	}
	for _, target := range targets {
		target.mu.Lock()
		target.policy = assignmentStoragePolicy{quotaBytes: command.QuotaBytes, minFreeBytes: command.MinFreeBytes}
		target.mu.Unlock()
	}
	return true, ""
}

func (executor *NativeExecutor) Snapshots() []*xirangv1.AssignmentUpdate {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	updates := make([]*xirangv1.AssignmentUpdate, 0, len(executor.ledger.Assignments))
	for _, record := range executor.ledger.Assignments {
		updates = append(updates, record.update())
	}
	return updates
}

func (executor *NativeExecutor) RunningCount() uint32 {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return uint32(len(executor.running))
}

func (executor *NativeExecutor) execute(parent context.Context, command *xirangv1.RunAssignment, running *runningAssignment) {
	key := assignmentKey(command.AssignmentId, command.Attempt)
	defer func() {
		identityReusable := true
		if running.workspace != "" {
			if err := sealAssignmentWorkspace(running.workspace); err != nil {
				identityReusable = false
				fmt.Fprintf(os.Stderr, "seal assignment workspace %s: %v\n", key, err)
			}
		}
		executor.mu.Lock()
		delete(executor.running, key)
		executor.mu.Unlock()
		if identityReusable && executor.uidPool != nil {
			executor.uidPool.Release(key)
		}
	}()
	if executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_PULLING, 0, "") != nil {
		return
	}
	workspace, executable, err := executor.stageArtifact(parent, command)
	if err != nil {
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
		return
	}
	select {
	case <-parent.Done():
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, parent.Err().Error())
		return
	case <-running.cancel:
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, "canceled before process start")
		return
	default:
	}
	workingDirectory, err := assignmentWorkingDirectory(workspace, command.WorkingDirectory)
	if err != nil {
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
		return
	}
	if command.StartBarrier {
		if err := writeAllocationHostfile(workspace, command.Hosts); err != nil {
			_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
			return
		}
		if err := writeAllocationManifest(workspace, command); err != nil {
			_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
			return
		}
	}
	var group *assignmentCgroup
	if executor.cgroups != nil {
		group, err = executor.cgroups.Prepare(command.AssignmentId, command.Attempt, command.Resources)
		if err != nil {
			_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
			return
		}
		defer func() { _ = group.Cleanup() }()
		// Record the path before the recursive ownership change. If chown fails
		// partway through, the deferred seal still prevents a later UID reuse
		// from exposing a partially prepared workspace.
		running.workspace = workspace
		if err := chownAssignmentWorkspace(workspace, running.credential); err != nil {
			_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
			return
		}
	}
	if command.StartBarrier {
		if err := executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_READY, 0, "waiting for gang release"); err != nil {
			return
		}
	}
	select {
	case <-parent.Done():
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, parent.Err().Error())
		return
	case <-running.cancel:
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, "canceled before gang release")
		return
	case <-running.release:
	}
	if command.RankCount > 1 {
		executor.executeLocalRanks(parent, command, running, workspace, workingDirectory, executable, group)
		return
	}
	rankExecutable, rankArguments, err := mpiAdapterInvocation(command, workspace, executable, command.Arguments, 0)
	if err != nil {
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
		return
	}
	process, release, err := executor.assignmentCommand(rankExecutable, rankArguments, running.credential)
	if err != nil {
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
		return
	}
	process.Dir = workingDirectory
	environment := command.Environment
	if group != nil && len(group.selectedDevices) > 0 {
		environment = environmentWithDevices(environment, group.selectedDevices)
	}
	process.Env = assignmentEnvironment(parallelEnvironment(environment, command, workspace, 0))
	stdoutLog, stderrLog := &tailBuffer{}, &tailBuffer{}
	process.Stdout, process.Stderr = stdoutLog, stderrLog
	if err := process.Start(); err != nil {
		closeExtraFiles(process)
		if release != nil {
			_ = release.Close()
		}
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, fmt.Sprintf("start native artifact: %v", err))
		return
	}
	closeExtraFiles(process)
	if group != nil {
		if err := group.AddProcess(process.Process.Pid); err != nil {
			_ = process.Process.Kill()
			_, _ = process.Process.Wait()
			_ = release.Close()
			_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, err.Error())
			return
		}
		if _, err := release.Write([]byte{1}); err != nil {
			_ = process.Process.Kill()
			_, _ = process.Process.Wait()
			_ = release.Close()
			_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, "release isolated task launcher: "+err.Error())
			return
		}
		_ = release.Close()
	}
	if err := executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, 0, ""); err != nil {
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		_, _ = process.Process.Wait()
		return
	}
	logDone := make(chan struct{})
	go executor.publishLogSnapshots(command.AssignmentId, command.Attempt, stdoutLog, stderrLog, logDone)
	defer close(logDone)
	usageStop := make(chan struct{})
	usageDone := make(chan struct{})
	go executor.monitorAssignmentUsage(command, running, workspace, group, []int{process.Process.Pid}, usageStop, usageDone)
	var stopUsageOnce sync.Once
	stopUsage := func() {
		stopUsageOnce.Do(func() {
			close(usageStop)
			<-usageDone
		})
	}
	defer stopUsage()
	waited := make(chan error, 1)
	go func() { waited <- process.Wait() }()
	var timeout <-chan time.Time
	var timer *time.Timer
	if command.Timeout != nil && command.Timeout.AsDuration() > 0 {
		timer = time.NewTimer(command.Timeout.AsDuration())
		timeout = timer.C
		defer timer.Stop()
	}
	select {
	case err := <-waited:
		stopUsage()
		if !executor.ensureCgroupEmpty(key, group) {
			return
		}
		executor.finishProcess(key, err, stdoutLog, stderrLog)
	case grace := <-running.cancel:
		stopUsage()
		terminateProcessGroup(process.Process.Pid, waited, grace)
		if !executor.ensureCgroupEmpty(key, group) {
			return
		}
		_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, "assignment canceled", stdoutLog, stderrLog)
	case <-timeout:
		stopUsage()
		terminateProcessGroup(process.Process.Pid, waited, 5*time.Second)
		if !executor.ensureCgroupEmpty(key, group) {
			return
		}
		_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, 0, "assignment timed out", stdoutLog, stderrLog)
	case <-parent.Done():
		stopUsage()
		terminateProcessGroup(process.Process.Pid, waited, 5*time.Second)
		if !executor.ensureCgroupEmpty(key, group) {
			return
		}
		_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, parent.Err().Error(), stdoutLog, stderrLog)
	}
}

type localRankProcess struct {
	localRank uint32
	command   *exec.Cmd
	release   *os.File
}

type localRankResult struct {
	localRank uint32
	err       error
}

func (executor *NativeExecutor) executeLocalRanks(
	parent context.Context,
	command *xirangv1.RunAssignment,
	running *runningAssignment,
	workspace, workingDirectory, executable string,
	group *assignmentCgroup,
) {
	key := assignmentKey(command.AssignmentId, command.Attempt)
	stdoutLog, stderrLog := &tailBuffer{}, &tailBuffer{}
	processes := make([]localRankProcess, 0, command.RankCount)
	startFailure := func(reason string) {
		for _, process := range processes {
			_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGKILL)
			_, _ = process.command.Process.Wait()
			if process.release != nil {
				_ = process.release.Close()
			}
		}
		_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, -1, reason, stdoutLog, stderrLog)
	}
	for localRank := uint32(0); localRank < command.RankCount; localRank++ {
		rankExecutable, rankArguments, err := mpiAdapterInvocation(command, workspace, executable, command.Arguments, localRank)
		if err != nil {
			startFailure(fmt.Sprintf("prepare local rank %d: %v", localRank, err))
			return
		}
		process, release, err := executor.assignmentCommand(rankExecutable, rankArguments, running.credential)
		if err != nil {
			startFailure(fmt.Sprintf("prepare local rank %d: %v", localRank, err))
			return
		}
		process.Dir = workingDirectory
		environment := command.Environment
		if group != nil && len(group.selectedDevices) > 0 {
			environment = environmentWithDevices(environment, group.selectedDevices)
		}
		process.Env = assignmentEnvironment(parallelEnvironment(environment, command, workspace, localRank))
		process.Stdout, process.Stderr = stdoutLog, stderrLog
		if err := process.Start(); err != nil {
			closeExtraFiles(process)
			if release != nil {
				_ = release.Close()
			}
			startFailure(fmt.Sprintf("start local rank %d: %v", localRank, err))
			return
		}
		closeExtraFiles(process)
		if group != nil {
			if err := group.AddProcess(process.Process.Pid); err != nil {
				_ = process.Process.Kill()
				_, _ = process.Process.Wait()
				_ = release.Close()
				startFailure(fmt.Sprintf("admit local rank %d to cgroup: %v", localRank, err))
				return
			}
			if _, err := release.Write([]byte{1}); err != nil {
				_ = process.Process.Kill()
				_, _ = process.Process.Wait()
				_ = release.Close()
				startFailure(fmt.Sprintf("release local rank %d launcher: %v", localRank, err))
				return
			}
			_ = release.Close()
		}
		processes = append(processes, localRankProcess{localRank: localRank, command: process, release: release})
	}
	if err := executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, 0, ""); err != nil {
		startFailure(err.Error())
		return
	}
	logDone := make(chan struct{})
	go executor.publishLogSnapshots(command.AssignmentId, command.Attempt, stdoutLog, stderrLog, logDone)
	defer close(logDone)
	usageStop, usageDone := make(chan struct{}), make(chan struct{})
	processIDs := make([]int, len(processes))
	for index := range processes {
		processIDs[index] = processes[index].command.Process.Pid
	}
	go executor.monitorAssignmentUsage(command, running, workspace, group, processIDs, usageStop, usageDone)
	stopUsage := sync.OnceFunc(func() { close(usageStop); <-usageDone })
	defer stopUsage()
	results := make(chan localRankResult, len(processes))
	for _, process := range processes {
		go func(process localRankProcess) {
			results <- localRankResult{localRank: process.localRank, err: process.command.Wait()}
		}(process)
	}
	var timeout <-chan time.Time
	var timer *time.Timer
	if command.Timeout != nil && command.Timeout.AsDuration() > 0 {
		timer = time.NewTimer(command.Timeout.AsDuration())
		timeout = timer.C
		defer timer.Stop()
	}
	remaining := len(processes)
	var firstFailure *localRankResult
	terminate := func(signal syscall.Signal) {
		for _, process := range processes {
			_ = syscall.Kill(-process.command.Process.Pid, signal)
		}
	}
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if result.err != nil && firstFailure == nil {
				copy := result
				firstFailure = &copy
				terminate(syscall.SIGKILL)
			}
		case grace := <-running.cancel:
			stopUsage()
			terminate(syscall.SIGTERM)
			if grace > 0 {
				deadline := time.NewTimer(grace)
				select {
				case <-deadline.C:
					terminate(syscall.SIGKILL)
				case <-parent.Done():
					if !deadline.Stop() {
						<-deadline.C
					}
					terminate(syscall.SIGKILL)
				}
			} else {
				terminate(syscall.SIGKILL)
			}
			for remaining > 0 {
				<-results
				remaining--
			}
			_ = executor.ensureCgroupEmpty(key, group)
			_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, "gang member canceled", stdoutLog, stderrLog)
			return
		case <-timeout:
			stopUsage()
			terminate(syscall.SIGKILL)
			for remaining > 0 {
				<-results
				remaining--
			}
			_ = executor.ensureCgroupEmpty(key, group)
			_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, -1, "gang member timed out", stdoutLog, stderrLog)
			return
		case <-parent.Done():
			stopUsage()
			terminate(syscall.SIGKILL)
			for remaining > 0 {
				<-results
				remaining--
			}
			_ = executor.ensureCgroupEmpty(key, group)
			_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED, 0, parent.Err().Error(), stdoutLog, stderrLog)
			return
		}
	}
	stopUsage()
	if !executor.ensureCgroupEmpty(key, group) {
		return
	}
	if firstFailure != nil {
		exitCode := int32(-1)
		var exitError *exec.ExitError
		if errors.As(firstFailure.err, &exitError) {
			exitCode = int32(exitError.ExitCode())
		}
		_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, exitCode,
			fmt.Sprintf("local rank %d failed: %v", firstFailure.localRank, firstFailure.err), stdoutLog, stderrLog)
		return
	}
	_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, 0, "", stdoutLog, stderrLog)
}

func (executor *NativeExecutor) monitorAssignmentUsage(
	command *xirangv1.RunAssignment,
	running *runningAssignment,
	workspace string,
	group *assignmentCgroup,
	processIDs []int,
	stop <-chan struct{},
	done chan<- struct{},
) {
	defer close(done)
	previous := time.Now()
	var memoryByteSeconds, gpuDeviceSeconds, wallTimeMillis uint64
	var suspended bool
	gpuDevices := requestedGPUDevices(command.Resources)
	sample := func(now time.Time) {
		elapsed := now.Sub(previous)
		if elapsed < 0 {
			elapsed = 0
		}
		previous = now
		memoryCurrent, _ := group.MemoryCurrentBytes()
		memoryByteSeconds = saturatingUsageAdd(memoryByteSeconds, saturatingUsageMultiply(memoryCurrent, uint64(elapsed/time.Second)))
		if !suspended {
			gpuDeviceSeconds = saturatingUsageAdd(gpuDeviceSeconds, saturatingUsageMultiply(gpuDevices, uint64(elapsed/time.Second)))
			wallTimeMillis = saturatingUsageAdd(wallTimeMillis, uint64(elapsed/time.Millisecond))
		}
		cpuUsage, _ := group.CPUUsageMicros()
		workspaceBytes, _ := assignmentWorkspaceSize(workspace)
		available, total := assignmentFilesystemCapacity(workspace)
		running.mu.RLock()
		policy := running.policy
		running.mu.RUnlock()
		minFree := policy.minFreeBytes
		if minFree == 0 {
			minFree = defaultStorageReserve(total)
		}
		reason := ""
		if policy.quotaBytes > 0 && workspaceBytes > policy.quotaBytes {
			reason = fmt.Sprintf("suspended: user group storage quota exceeded (used %d bytes, quota %d bytes)", workspaceBytes, policy.quotaBytes)
		} else if minFree > 0 && available < minFree {
			reason = fmt.Sprintf("suspended: node filesystem available space is below the safety reserve (available %d bytes, reserve %d bytes)", available, minFree)
		}
		key := assignmentKey(command.AssignmentId, command.Attempt)
		if reason != "" && !suspended {
			if signalProcessGroups(processIDs, syscall.SIGSTOP) {
				suspended = true
				_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_SUSPENDED, 0, reason)
			}
		} else if reason == "" && suspended {
			if signalProcessGroups(processIDs, syscall.SIGCONT) {
				suspended = false
				_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, 0, "")
			}
		}
		executor.publishUsage(&xirangv1.AssignmentUsageSnapshot{
			AssignmentId: command.AssignmentId, Attempt: command.Attempt,
			CpuUsageMicros: cpuUsage, MemoryByteSeconds: memoryByteSeconds,
			GpuDeviceSeconds: gpuDeviceSeconds, WallTimeMillis: wallTimeMillis,
			WorkspaceBytes: workspaceBytes, FilesystemAvailableBytes: available, FilesystemTotalBytes: total,
			ObservedAt: timestamppb.New(now.UTC()),
		})
	}
	ticker := time.NewTicker(assignmentUsageInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			sample(time.Now())
			if suspended {
				_ = signalProcessGroups(processIDs, syscall.SIGCONT)
			}
			return
		case now := <-ticker.C:
			sample(now)
		}
	}
}

func signalProcessGroups(processIDs []int, signal syscall.Signal) bool {
	succeeded := len(processIDs) > 0
	for _, processID := range processIDs {
		if syscall.Kill(-processID, signal) != nil {
			succeeded = false
		}
	}
	return succeeded
}

func requestedGPUDevices(resources *xirangv1.ResourceRequest) uint64 {
	if resources == nil {
		return 0
	}
	var count uint64
	for name, quantity := range resources.Devices {
		resource := strings.ToLower(name)
		parts := strings.Split(resource, "/")
		isGPU := parts[len(parts)-1] == "gpu" || resource == "xirang.io/drm-render"
		if quantity > 0 && isGPU {
			count = saturatingUsageAdd(count, uint64(quantity))
		}
	}
	return count
}

func assignmentWorkspaceSize(root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total = saturatingUsageAdd(total, uint64(info.Size()))
		return nil
	})
	return total, err
}

func assignmentFilesystemCapacity(root string) (uint64, uint64) {
	var stats unix.Statfs_t
	if err := unix.Statfs(root, &stats); err != nil || stats.Bsize <= 0 {
		return 0, 0
	}
	blockSize := uint64(stats.Bsize)
	return saturatingUsageMultiply(stats.Bavail, blockSize), saturatingUsageMultiply(stats.Blocks, blockSize)
}

func defaultStorageReserve(total uint64) uint64 {
	if total == 0 {
		return 0
	}
	reserve := total / 50
	if reserve < 256<<20 {
		reserve = 256 << 20
	}
	if reserve > 4<<30 {
		reserve = 4 << 30
	}
	return reserve
}

func saturatingUsageAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func saturatingUsageMultiply(left, right uint64) uint64 {
	if left != 0 && right > ^uint64(0)/left {
		return ^uint64(0)
	}
	return left * right
}

func (executor *NativeExecutor) publishLogSnapshots(assignmentID string, attempt uint32, stdoutLog, stderrLog *tailBuffer, done <-chan struct{}) {
	ticker := time.NewTicker(assignmentLogInterval)
	defer ticker.Stop()
	var previous [sha256.Size]byte
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			stdout, stdoutTruncated := stdoutLog.snapshot()
			stderr, stderrTruncated := stderrLog.snapshot()
			if len(stdout) == 0 && len(stderr) == 0 {
				continue
			}
			digestInput := append(append(append([]byte(nil), stdout...), 0), stderr...)
			if stdoutTruncated || stderrTruncated {
				digestInput = append(digestInput, 1)
			}
			digest := sha256.Sum256(digestInput)
			if digest == previous {
				continue
			}
			previous = digest
			executor.publishLog(&xirangv1.AssignmentLogSnapshot{
				AssignmentId: assignmentID, Attempt: attempt, StdoutTail: stdout, StderrTail: stderr,
				LogsTruncated: stdoutTruncated || stderrTruncated, ObservedAt: timestamppb.Now(),
			})
		}
	}
}

func (executor *NativeExecutor) ensureCgroupEmpty(key string, group *assignmentCgroup) bool {
	if group == nil {
		return true
	}
	if err := group.KillAll(); err != nil {
		_ = executor.transition(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, 0, "cgroup cleanup failed; reservation retained: "+err.Error())
		return false
	}
	return true
}

func (executor *NativeExecutor) finishProcess(key string, err error, stdoutLog, stderrLog *tailBuffer) {
	if err == nil {
		_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED, 0, "", stdoutLog, stderrLog)
		return
	}
	exitCode := int32(-1)
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitCode = int32(exitError.ExitCode())
	}
	_ = executor.transitionWithLogs(key, xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED, exitCode, err.Error(), stdoutLog, stderrLog)
}

func (executor *NativeExecutor) transition(key string, state xirangv1.AssignmentState, exitCode int32, reason string) error {
	return executor.transitionWithLogs(key, state, exitCode, reason, nil, nil)
}

func (executor *NativeExecutor) transitionWithLogs(key string, state xirangv1.AssignmentState, exitCode int32, reason string, stdoutLog, stderrLog *tailBuffer) error {
	executor.mu.Lock()
	for index := range executor.ledger.Assignments {
		record := &executor.ledger.Assignments[index]
		if assignmentKey(record.AssignmentID, record.Attempt) != key {
			continue
		}
		record.State, record.ExitCode, record.Reason, record.OccurredAt = state, exitCode, reason, time.Now().UTC()
		if stdoutLog != nil && stderrLog != nil {
			var stdoutTruncated, stderrTruncated bool
			record.StdoutTail, stdoutTruncated = stdoutLog.snapshot()
			record.StderrTail, stderrTruncated = stderrLog.snapshot()
			record.LogsTruncated = stdoutTruncated || stderrTruncated
		}
		if err := executor.persistLocked(); err != nil {
			executor.mu.Unlock()
			return err
		}
		update := record.update()
		executor.mu.Unlock()
		executor.publish(update)
		return nil
	}
	executor.mu.Unlock()
	return errors.New("assignment state disappeared")
}

func (executor *NativeExecutor) stageArtifact(ctx context.Context, command *xirangv1.RunAssignment) (string, string, error) {
	workspace := filepath.Join(executor.workspaceDirectory, workspaceName(command.AssignmentId, command.Attempt))
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return "", "", err
	}
	entrypoint, err := safeRelativePath(command.Artifact.Entrypoint)
	if err != nil {
		return "", "", fmt.Errorf("invalid artifact entrypoint: %w", err)
	}
	destination := filepath.Join(workspace, entrypoint)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", "", err
	}
	cachePath := filepath.Join(executor.cacheDirectory, strings.ToLower(command.Artifact.Sha256))
	artifactCacheMu.Lock()
	err = executor.ensureCachedArtifact(ctx, command.Artifact.Uri, command.Artifact.Sha256, cachePath)
	artifactCacheMu.Unlock()
	if err != nil {
		return "", "", err
	}
	input, err := os.Open(cachePath)
	if err != nil {
		return "", "", err
	}
	defer input.Close()
	temporary := destination + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", err
	}
	written, copyErr := io.Copy(file, io.LimitReader(input, maxArtifactBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(temporary)
		return "", "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return "", "", closeErr
	}
	if written > maxArtifactBytes {
		_ = os.Remove(temporary)
		return "", "", errors.New("native artifact exceeds 1 GiB limit")
	}
	if err := os.Chmod(temporary, 0o700); err != nil {
		_ = os.Remove(temporary)
		return "", "", err
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return "", "", err
	}
	return workspace, destination, nil
}

func ensureCachedArtifact(ctx context.Context, uri, expectedHash, cachePath string) error {
	return ensureCachedArtifactWithOpener(ctx, uri, expectedHash, cachePath, openArtifact)
}

func (executor *NativeExecutor) ensureCachedArtifact(ctx context.Context, uri, expectedHash, cachePath string) error {
	return ensureCachedArtifactWithOpener(ctx, uri, expectedHash, cachePath, executor.openArtifact)
}

func ensureCachedArtifactWithOpener(ctx context.Context, uri, expectedHash, cachePath string, opener func(context.Context, string) (io.ReadCloser, error)) error {
	if info, err := os.Stat(cachePath); err == nil && info.Mode().IsRegular() && info.Size() <= maxArtifactBytes {
		valid, verifyErr := fileMatchesSHA256(cachePath, expectedHash)
		if verifyErr == nil && valid {
			return nil
		}
	}
	_ = os.Remove(cachePath)
	input, err := opener(ctx, uri)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := cachePath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(input, maxArtifactBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written > maxArtifactBytes {
		_ = os.Remove(temporary)
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return errors.New("native artifact exceeds 1 GiB limit")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedHash) {
		_ = os.Remove(temporary)
		return errors.New("native artifact SHA-256 mismatch")
	}
	if err := os.Rename(temporary, cachePath); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func fileMatchesSHA256(path, expectedHash string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxArtifactBytes+1)); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedHash), nil
}

func (executor *NativeExecutor) persistLocked() error {
	encoded, err := json.MarshalIndent(executor.ledger, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(executor.directory, assignmentStateFile), append(encoded, '\n'), 0o600)
}

func (executor *NativeExecutor) publish(update *xirangv1.AssignmentUpdate) {
	select {
	case executor.updates <- update:
	default:
		select {
		case <-executor.updates:
		default:
		}
		select {
		case executor.updates <- update:
		default:
		}
	}
}

func (executor *NativeExecutor) publishLog(snapshot *xirangv1.AssignmentLogSnapshot) {
	select {
	case executor.logs <- snapshot:
	default:
		select {
		case <-executor.logs:
		default:
		}
		select {
		case executor.logs <- snapshot:
		default:
		}
	}
}

func (executor *NativeExecutor) publishUsage(snapshot *xirangv1.AssignmentUsageSnapshot) {
	select {
	case executor.usage <- snapshot:
	default:
		select {
		case <-executor.usage:
		default:
		}
		select {
		case executor.usage <- snapshot:
		default:
		}
	}
}

func (record assignmentRecord) update() *xirangv1.AssignmentUpdate {
	return &xirangv1.AssignmentUpdate{
		AssignmentId: record.AssignmentID, Attempt: record.Attempt, State: record.State,
		ExitCode: record.ExitCode, Reason: record.Reason, OccurredAt: timestamppb.New(record.OccurredAt),
		StdoutTail: append([]byte(nil), record.StdoutTail...), StderrTail: append([]byte(nil), record.StderrTail...), LogsTruncated: record.LogsTruncated,
	}
}

func validateRunAssignment(command *xirangv1.RunAssignment) error {
	if command == nil || strings.TrimSpace(command.AssignmentId) == "" || command.Attempt == 0 {
		return errors.New("assignment ID and positive attempt are required")
	}
	if command.Artifact == nil || command.Artifact.Uri == "" || command.Artifact.Sha256 == "" || command.Artifact.Entrypoint == "" {
		return errors.New("native artifact URI, SHA-256, and entrypoint are required")
	}
	if len(command.Artifact.Sha256) != sha256.Size*2 {
		return errors.New("native artifact SHA-256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(command.Artifact.Sha256); err != nil {
		return errors.New("native artifact SHA-256 is invalid")
	}
	if command.Artifact.Architecture != "" && command.Artifact.Architecture != runtime.GOARCH {
		return fmt.Errorf("artifact architecture %q does not match node architecture %q", command.Artifact.Architecture, runtime.GOARCH)
	}
	if command.User != "" || command.Group != "" {
		return errors.New("per-assignment user and group require the Phase 3 privilege-isolation backend")
	}
	if command.Resources != nil && (command.Resources.MilliCpu < 0 || command.Resources.EphemeralStorageBytes < 0) {
		return errors.New("resource request contains unsupported or negative quantities")
	}
	if command.Resources != nil {
		if (command.Resources.CpuSet != "" && !validCPUList(command.Resources.CpuSet)) ||
			(command.Resources.NumaNodes != "" && !validCPUList(command.Resources.NumaNodes)) {
			return errors.New("resource request contains an invalid CPU or NUMA list")
		}
		for resource, quantity := range command.Resources.Devices {
			if resource == "" || len(resource) > 128 || quantity <= 0 || quantity > 1_000_000 {
				return errors.New("resource request contains an invalid device quantity")
			}
		}
	}
	if command.Timeout != nil && command.Timeout.AsDuration() < 0 {
		return errors.New("assignment timeout must not be negative")
	}
	if command.StartBarrier {
		if strings.TrimSpace(command.AllocationId) == "" || len(command.AllocationId) > 256 ||
			command.RankCount == 0 || command.RankCount > 256 || command.WorldSize == 0 || command.WorldSize > 4096 ||
			uint64(command.RankStart)+uint64(command.RankCount) > uint64(command.WorldSize) ||
			len(command.Hosts) < 2 || len(command.Hosts) > 128 || int(command.NodeRank) >= len(command.Hosts) ||
			!validParallelLauncher(command.Launcher) {
			return errors.New("gang assignment contains invalid allocation or rank metadata")
		}
		var slots uint64
		seen := make(map[string]bool, len(command.Hosts))
		for _, host := range command.Hosts {
			if host == nil || host.NodeId == "" || len(host.NodeId) > 256 || seen[host.NodeId] || host.Slots == 0 || host.Slots > 256 ||
				!boundedSingleLine(host.Hostname, 255, false) || !boundedSingleLine(host.Address, 255, false) ||
				!boundedSingleLine(host.NetworkInterface, 64, true) {
				return errors.New("gang assignment contains an invalid host")
			}
			seen[host.NodeId] = true
			slots += uint64(host.Slots)
		}
		if slots != uint64(command.WorldSize) {
			return errors.New("gang assignment host slots do not equal world size")
		}
	} else if command.AllocationId != "" || command.RankCount != 0 || command.WorldSize != 0 || len(command.Hosts) != 0 {
		return errors.New("parallel metadata requires a gang start barrier")
	}
	if _, err := safeRelativePath(command.Artifact.Entrypoint); err != nil {
		return fmt.Errorf("invalid artifact entrypoint: %w", err)
	}
	if command.WorkingDirectory != "" {
		if _, err := safeRelativePath(command.WorkingDirectory); err != nil {
			return fmt.Errorf("invalid working directory: %w", err)
		}
	}
	for name, value := range command.Environment {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment variable %q", name)
		}
	}
	return nil
}

func (executor *NativeExecutor) validateRunAssignment(command *xirangv1.RunAssignment) error {
	if !executor.allowRoot && os.Geteuid() == 0 {
		return errors.New("root Agent execution is disabled until the Phase 3 privilege-isolation backend is configured")
	}
	if command != nil && command.Resources != nil && (command.Resources.MilliCpu != 0 || command.Resources.MemoryBytes != 0) {
		if executor.cgroups == nil || executor.credentialTemplate == nil || executor.uidPool == nil {
			return errors.New("resource limits require the cgroup v2 isolation backend")
		}
		if command.Resources.MilliCpu <= 0 || command.Resources.MemoryBytes == 0 {
			return errors.New("cgroup execution requires positive CPU and memory limits")
		}
	}
	if executor.cgroups != nil && (command == nil || command.Resources == nil) {
		return errors.New("isolated execution requires an explicit resource request")
	}
	if command != nil && command.Resources != nil {
		if (command.Resources.CpuSet != "" || command.Resources.NumaNodes != "") && (executor.cgroups == nil || !executor.cgroups.SupportsCPUSet()) {
			return errors.New("CPU/NUMA affinity requires the delegated cpuset controller")
		}
		if len(command.Resources.Devices) > 0 && (executor.cgroups == nil || !executor.cgroups.SupportsDeviceEnforcement()) {
			return errors.New("named devices require cgroup device enforcement")
		}
	}
	if len(executor.artifactTrustKeys) > 0 {
		if command == nil || command.Artifact == nil || len(command.Artifact.Signature) != ed25519.SignatureSize {
			return errors.New("artifact signature from a configured trusted build key is required")
		}
		digest, err := hex.DecodeString(command.Artifact.Sha256)
		if err != nil {
			return errors.New("artifact digest is invalid")
		}
		trusted := false
		for _, key := range executor.artifactTrustKeys {
			if ed25519.Verify(key, digest, command.Artifact.Signature) {
				trusted = true
				break
			}
		}
		if !trusted {
			return errors.New("artifact signature is not trusted")
		}
	}
	return validateRunAssignment(command)
}

func (executor *NativeExecutor) SupportsCPUSet() bool {
	return executor != nil && executor.cgroups != nil && executor.cgroups.SupportsCPUSet()
}

func (executor *NativeExecutor) SupportsDeviceEnforcement() bool {
	return executor != nil && executor.cgroups != nil && executor.cgroups.SupportsDeviceEnforcement() && len(executor.deviceResources) > 0
}

func (executor *NativeExecutor) DeviceResources() []string {
	return append([]string(nil), executor.deviceResources...)
}

func environmentWithDevices(base map[string]string, devices map[string][]string) map[string]string {
	cloned := make(map[string]string, len(base)+2)
	for key, value := range base {
		cloned[key] = value
	}
	encoded, _ := json.Marshal(devices)
	cloned["XIRANG_ASSIGNED_DEVICES"] = string(encoded)
	if _, exists := cloned["CUDA_VISIBLE_DEVICES"]; !exists {
		var minors []string
		for _, path := range devices["nvidia.com/gpu"] {
			name := filepath.Base(path)
			if strings.HasPrefix(name, "nvidia") {
				minors = append(minors, strings.TrimPrefix(name, "nvidia"))
			}
		}
		if len(minors) > 0 {
			cloned["CUDA_VISIBLE_DEVICES"] = strings.Join(minors, ",")
		}
	}
	return cloned
}

func (executor *NativeExecutor) assignmentCommand(executable string, arguments []string, credential *syscall.Credential) (*exec.Cmd, *os.File, error) {
	attributes := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if executor.cgroups == nil {
		command := exec.Command(executable, arguments...)
		command.SysProcAttr = attributes
		return command, nil, nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	if credential == nil || credential.Uid == 0 {
		return nil, nil, errors.New("temporary non-root task credential is required")
	}
	attributes.Credential = credential
	command := exec.Command("/proc/self/exe", append([]string{"__xirang-exec", executable}, arguments...)...)
	command.ExtraFiles = []*os.File{reader}
	command.SysProcAttr = attributes
	return command, writer, nil
}

func chownAssignmentWorkspace(workspace string, credential *syscall.Credential) error {
	if credential == nil || credential.Uid == 0 {
		return errors.New("dedicated non-root task credentials are required")
	}
	return filepath.Walk(workspace, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Lchown never follows a hostile or stale symlink out of the private
		// workspace while the privileged Agent prepares ownership.
		if err := os.Lchown(path, int(credential.Uid), int(credential.Gid)); err != nil {
			return fmt.Errorf("assign task workspace ownership: %w", err)
		}
		return nil
	})
}

func sealAssignmentWorkspace(workspace string) error {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return errors.New("assignment workspace path is invalid")
	}
	if err := os.Chown(workspace, 0, 0); err != nil {
		return err
	}
	return os.Chmod(workspace, 0o700)
}

func sealStaleAssignmentWorkspaces(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := sealAssignmentWorkspace(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func closeExtraFiles(command *exec.Cmd) {
	for _, file := range command.ExtraFiles {
		_ = file.Close()
	}
}

// RunExecutorLauncher waits until the parent has attached this process to its
// cgroup, then atomically replaces itself with the verified native artifact.
func RunExecutorLauncher(arguments []string) error {
	if len(arguments) == 0 || !filepath.IsAbs(arguments[0]) {
		return errors.New("isolated executor launcher requires an absolute executable path")
	}
	gate := os.NewFile(3, "xirang-executor-gate")
	if gate == nil {
		return errors.New("isolated executor gate is unavailable")
	}
	defer gate.Close()
	buffer := []byte{0}
	if count, err := gate.Read(buffer); err != nil || count != 1 || buffer[0] != 1 {
		return errors.New("isolated executor was not admitted to its cgroup")
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("disable task privilege gains: %w", err)
	}
	syscall.Umask(0o077)
	return syscall.Exec(arguments[0], append([]string{arguments[0]}, arguments[1:]...), os.Environ())
}

func openArtifact(ctx context.Context, rawURI string) (io.ReadCloser, error) {
	return openArtifactWithClient(ctx, rawURI, nil, nil)
}

func (executor *NativeExecutor) openArtifact(ctx context.Context, rawURI string) (io.ReadCloser, error) {
	executor.artifactMu.RLock()
	controller, client := executor.artifactController, executor.artifactHTTPClient
	executor.artifactMu.RUnlock()
	return openArtifactWithClient(ctx, rawURI, controller, client)
}

func openArtifactWithClient(ctx context.Context, rawURI string, controller *url.URL, controllerClient *http.Client) (io.ReadCloser, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return nil, fmt.Errorf("parse native artifact URI: %w", err)
	}
	switch parsed.Scheme {
	case "file":
		if parsed.Host != "" && parsed.Host != "localhost" {
			return nil, errors.New("file artifact URI must refer to the local host")
		}
		return os.Open(parsed.Path)
	case "https":
		if parsed.Host == "" || parsed.User != nil {
			return nil, errors.New("HTTPS artifact URI has invalid authority")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURI, nil)
		if err != nil {
			return nil, err
		}
		client := publicArtifactHTTPClient()
		if controller != nil && parsed.Scheme == controller.Scheme && strings.EqualFold(parsed.Host, controller.Host) {
			if controllerClient == nil {
				return nil, errors.New("controller artifact requires the enrolled Agent identity")
			}
			client = controllerClient
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("download native artifact: HTTP %s", response.Status)
		}
		return response.Body, nil
	default:
		return nil, errors.New("native artifact URI must use file:// or https://")
	}
}

func publicArtifactHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			address = address.Unmap()
			if publicArtifactAddress(address.String()) {
				return dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			}
		}
		return nil, errors.New("artifact host resolves only to private or non-routable addresses")
	}}
	return &http.Client{Timeout: 5 * time.Minute, Transport: transport, CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		if request.URL.Scheme != "https" || request.URL.User != nil {
			return errors.New("native artifact redirect must remain on authenticated HTTPS")
		}
		return nil
	}}
}

func publicArtifactAddress(raw string) bool {
	address, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	// IsGlobalUnicast intentionally includes documentation, benchmark, protocol
	// assignment, and future-use ranges. They are not valid public artifact
	// origins and must not become a path to local routing policy.
	blocked := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001:10::/28"),
	}
	for _, prefix := range blocked {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func assignmentWorkingDirectory(workspace, requested string) (string, error) {
	if requested == "" {
		return workspace, nil
	}
	relative, err := safeRelativePath(requested)
	if err != nil {
		return "", err
	}
	directory := filepath.Join(workspace, relative)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	return directory, nil
}

func assignmentEnvironment(environment map[string]string) []string {
	result := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func parallelEnvironment(base map[string]string, command *xirangv1.RunAssignment, workspace string, localRank uint32) map[string]string {
	result := make(map[string]string, len(base)+20)
	for name, value := range base {
		result[name] = value
	}
	if command == nil || !command.StartBarrier {
		return result
	}
	rank := command.RankStart + localRank
	values := map[string]string{
		"XIRANG_ALLOCATION_ID":     command.AllocationId,
		"XIRANG_NODE_RANK":         strconv.FormatUint(uint64(command.NodeRank), 10),
		"XIRANG_RANK":              strconv.FormatUint(uint64(rank), 10),
		"XIRANG_LOCAL_RANK":        strconv.FormatUint(uint64(localRank), 10),
		"XIRANG_WORLD_SIZE":        strconv.FormatUint(uint64(command.WorldSize), 10),
		"XIRANG_LOCAL_SIZE":        strconv.FormatUint(uint64(command.RankCount), 10),
		"XIRANG_LAUNCHER":          command.Launcher,
		"XIRANG_HOSTFILE":          filepath.Join(workspace, ".xirang", "hosts"),
		"XIRANG_NETWORK_INTERFACE": command.NetworkInterface,
		"XIRANG_PRIMARY_ADDRESS":   command.PrimaryAddress,
	}
	if command.Launcher == "openmpi" {
		values["OMPI_COMM_WORLD_SIZE"] = values["XIRANG_WORLD_SIZE"]
		values["OMPI_COMM_WORLD_RANK"] = values["XIRANG_RANK"]
		values["OMPI_COMM_WORLD_LOCAL_SIZE"] = values["XIRANG_LOCAL_SIZE"]
		values["OMPI_COMM_WORLD_LOCAL_RANK"] = values["XIRANG_LOCAL_RANK"]
		values["OMPI_COMM_WORLD_NODE_RANK"] = values["XIRANG_NODE_RANK"]
	} else if command.Launcher == "mpich" || command.Launcher == "intelmpi" {
		values["PMI_SIZE"] = values["XIRANG_WORLD_SIZE"]
		values["PMI_RANK"] = values["XIRANG_RANK"]
		values["MPI_LOCALNRANKS"] = values["XIRANG_LOCAL_SIZE"]
		values["MPI_LOCALRANKID"] = values["XIRANG_LOCAL_RANK"]
		if command.Launcher == "intelmpi" {
			values["I_MPI_PIN_RESPECT_CPUSET"] = "1"
		}
	}
	for name, value := range values {
		result[name] = value
	}
	return result
}

func writeAllocationHostfile(workspace string, hosts []*xirangv1.AllocationHost) error {
	directory := filepath.Join(workspace, ".xirang")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create allocation metadata directory: %w", err)
	}
	var content strings.Builder
	for _, host := range hosts {
		name := host.Address
		if name == "" {
			name = host.Hostname
		}
		if !boundedSingleLine(name, 255, false) || host.Slots == 0 || host.Slots > 256 {
			return errors.New("allocation hostfile contains an invalid host")
		}
		fmt.Fprintf(&content, "%s slots=%d\n", name, host.Slots)
	}
	return atomicWrite(filepath.Join(directory, "hosts"), []byte(content.String()), 0o600)
}

type allocationManifest struct {
	Version          string                     `json:"version"`
	AllocationID     string                     `json:"allocationId"`
	JobID            string                     `json:"jobId"`
	AssignmentID     string                     `json:"assignmentId"`
	Attempt          uint32                     `json:"attempt"`
	Launcher         string                     `json:"launcher"`
	NodeRank         uint32                     `json:"nodeRank"`
	RankStart        uint32                     `json:"rankStart"`
	RankCount        uint32                     `json:"rankCount"`
	WorldSize        uint32                     `json:"worldSize"`
	NetworkInterface string                     `json:"networkInterface,omitempty"`
	PrimaryAddress   string                     `json:"primaryAddress,omitempty"`
	Hosts            []*xirangv1.AllocationHost `json:"hosts"`
}

func writeAllocationManifest(workspace string, command *xirangv1.RunAssignment) error {
	if command == nil {
		return errors.New("allocation manifest requires an assignment")
	}
	encoded, err := json.Marshal(allocationManifest{
		Version: "xirang-allocation-v1", AllocationID: command.AllocationId, JobID: command.JobId,
		AssignmentID: command.AssignmentId, Attempt: command.Attempt, Launcher: command.Launcher,
		NodeRank: command.NodeRank, RankStart: command.RankStart, RankCount: command.RankCount,
		WorldSize: command.WorldSize, NetworkInterface: command.NetworkInterface,
		PrimaryAddress: command.PrimaryAddress, Hosts: command.Hosts,
	})
	if err != nil {
		return fmt.Errorf("encode allocation manifest: %w", err)
	}
	return atomicWrite(filepath.Join(workspace, ".xirang", "allocation.json"), encoded, 0o600)
}

func mpiAdapterName(launcher string) string { return "xirang-mpi-adapter-" + launcher }

func mpiAdapterInvocation(command *xirangv1.RunAssignment, workspace, executable string, arguments []string, localRank uint32) (string, []string, error) {
	if command == nil || command.Launcher == "" || command.Launcher == "native" {
		return executable, arguments, nil
	}
	if !validParallelLauncher(command.Launcher) {
		return "", nil, errors.New("unsupported parallel launcher")
	}
	adapter, err := exec.LookPath(mpiAdapterName(command.Launcher))
	if err != nil {
		return "", nil, fmt.Errorf("certified %s adapter is unavailable", command.Launcher)
	}
	adapterArguments := []string{
		"--xirang-manifest", filepath.Join(workspace, ".xirang", "allocation.json"),
		"--xirang-local-rank", strconv.FormatUint(uint64(localRank), 10), "--", executable,
	}
	adapterArguments = append(adapterArguments, arguments...)
	return adapter, adapterArguments, nil
}

func validParallelLauncher(value string) bool {
	return value == "native" || value == "openmpi" || value == "mpich" || value == "intelmpi"
}

func boundedSingleLine(value string, maximum int, emptyAllowed bool) bool {
	return (emptyAllowed || value != "") && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n\t ")
}

func safeRelativePath(path string) (string, error) {
	cleaned := filepath.Clean(path)
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("path must be a non-empty relative path inside the assignment workspace")
	}
	return cleaned, nil
}

func assignmentKey(id string, attempt uint32) string { return fmt.Sprintf("%s/%d", id, attempt) }

func workspaceName(id string, attempt uint32) string {
	digest := sha256.Sum256([]byte(assignmentKey(id, attempt)))
	return hex.EncodeToString(digest[:])
}

func assignmentTerminal(state xirangv1.AssignmentState) bool {
	return state == xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED ||
		state == xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED ||
		state == xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED
}

func terminateProcessGroup(pid int, waited <-chan error, grace time.Duration) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if grace <= 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-waited
		return
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-waited:
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-waited
	}
}
