package build

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var safeWorkerValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

type RemoteCommandQueue interface {
	Enqueue(string, *xirangv1.ControlMessage) error
	NodeActive(string) bool
}

type remoteResult struct {
	logs   string
	digest string
	err    error
}

type remotePending struct {
	nodeID string
	result chan remoteResult
}

// RemotePool is both a dynamic RunnerRegistry and the terminal-event sink for
// builds executed by Agents over their existing mTLS control stream.
type RemotePool struct {
	commands RemoteCommandQueue
	mu       sync.RWMutex
	workers  map[string]domain.BuildWorker
	pending  map[string]remotePending
}

func NewRemotePool(commands RemoteCommandQueue) *RemotePool {
	return &RemotePool{commands: commands, workers: make(map[string]domain.BuildWorker), pending: make(map[string]remotePending)}
}

func (pool *RemotePool) ReportWorker(nodeID string, report *xirangv1.NativeBuildWorker) error {
	if nodeID == "" || report == nil || !safeWorkerValue.MatchString(report.Architecture) ||
		len(report.Toolchains) == 0 || len(report.Toolchains) > 32 || len(report.Isolation) > 256 {
		return ErrInvalidRequest
	}
	toolchains := make([]string, 0, len(report.Toolchains))
	seen := make(map[string]bool)
	for _, toolchain := range report.Toolchains {
		if !safeWorkerValue.MatchString(toolchain) {
			return ErrInvalidRequest
		}
		if !seen[toolchain] {
			seen[toolchain] = true
			toolchains = append(toolchains, toolchain)
		}
	}
	sort.Strings(toolchains)
	pool.mu.Lock()
	pool.workers[nodeID] = domain.BuildWorker{
		ID: "agent:" + nodeID, Architecture: report.Architecture, Toolchains: toolchains,
		ToolchainCapabilities: capabilitiesFromReport(report.ToolchainCapabilities),
		Isolation:             report.Isolation + "+mTLS-control-stream", Ready: report.Ready,
		SupportsCancellation: report.SupportsCancellation,
	}
	pool.mu.Unlock()
	return nil
}

func (pool *RemotePool) RemoveWorker(nodeID string) {
	pool.mu.Lock()
	delete(pool.workers, nodeID)
	for buildID, pending := range pool.pending {
		if pending.nodeID == nodeID {
			delete(pool.pending, buildID)
			select {
			case pending.result <- remoteResult{err: errors.New("remote build worker disconnected")}:
			default:
			}
		}
	}
	pool.mu.Unlock()
}

func (pool *RemotePool) ReportUpdate(nodeID string, update *xirangv1.NativeBuildUpdate) error {
	if update == nil || update.BuildId == "" || update.OccurredAt == nil || len(update.LogTail) > 64<<10 || len(update.Failure) > 4096 ||
		(update.OutputSha256 != "" && !validSHA256(update.OutputSha256)) {
		return ErrInvalidRequest
	}
	if err := update.OccurredAt.CheckValid(); err != nil {
		return ErrInvalidRequest
	}
	if update.State == xirangv1.NativeBuildState_NATIVE_BUILD_STATE_RUNNING {
		return nil
	}
	if update.State != xirangv1.NativeBuildState_NATIVE_BUILD_STATE_SUCCEEDED &&
		update.State != xirangv1.NativeBuildState_NATIVE_BUILD_STATE_FAILED &&
		update.State != xirangv1.NativeBuildState_NATIVE_BUILD_STATE_CANCELED {
		return ErrInvalidRequest
	}
	if update.State == xirangv1.NativeBuildState_NATIVE_BUILD_STATE_SUCCEEDED && update.OutputSha256 == "" {
		return ErrInvalidRequest
	}
	pool.mu.Lock()
	pending, ok := pool.pending[update.BuildId]
	if ok && pending.nodeID == nodeID {
		delete(pool.pending, update.BuildId)
	}
	pool.mu.Unlock()
	if !ok || pending.nodeID != nodeID {
		return nil
	}
	result := remoteResult{logs: update.LogTail, digest: update.OutputSha256}
	switch update.State {
	case xirangv1.NativeBuildState_NATIVE_BUILD_STATE_SUCCEEDED:
	case xirangv1.NativeBuildState_NATIVE_BUILD_STATE_FAILED:
		if update.Failure == "" {
			update.Failure = "remote native build failed"
		}
		result.err = errors.New(update.Failure)
	case xirangv1.NativeBuildState_NATIVE_BUILD_STATE_CANCELED:
		result.err = context.Canceled
	}
	pending.result <- result
	return nil
}

func (pool *RemotePool) Workers() []domain.BuildWorker {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	workers := make([]domain.BuildWorker, 0, len(pool.workers))
	for nodeID, worker := range pool.workers {
		worker.Ready = worker.Ready && pool.commands != nil && pool.commands.NodeActive(nodeID)
		workers = append(workers, worker)
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
	return workers
}

func (pool *RemotePool) Select(recipe domain.BuildRecipe) (Runner, bool) {
	builderArchitecture := recipe.BuilderArchitecture
	if builderArchitecture == "" {
		builderArchitecture = recipe.Architecture
	}
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	for nodeID, worker := range pool.workers {
		if worker.Ready && worker.Architecture == builderArchitecture && (recipe.WorkerID == "" || recipe.WorkerID == worker.ID) && workerSupports(worker, recipe) &&
			pool.commands != nil && pool.commands.NodeActive(nodeID) {
			return &remoteRunner{pool: pool, nodeID: nodeID, worker: worker}, true
		}
	}
	return nil, false
}

func capabilitiesFromReport(items []*xirangv1.ToolchainCapability) []domain.ToolchainCapability {
	result := make([]domain.ToolchainCapability, 0, len(items))
	for _, item := range items {
		if item == nil || !safeWorkerValue.MatchString(item.Name) || len(item.Version) > 160 || len(item.TargetArchitectures) > 32 || len(item.Executables) > 32 {
			continue
		}
		valid := true
		for _, value := range append(append([]string(nil), item.TargetArchitectures...), item.Executables...) {
			if !safeWorkerValue.MatchString(value) {
				valid = false
				break
			}
		}
		if valid && len(item.TargetArchitectures) > 0 {
			result = append(result, domain.ToolchainCapability{Name: item.Name, Version: item.Version, TargetArchitectures: uniqueSorted(item.TargetArchitectures), Executables: uniqueSorted(item.Executables)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

type remoteRunner struct {
	pool     *RemotePool
	nodeID   string
	worker   domain.BuildWorker
	digestMu sync.Mutex
	digest   string
}

func (runner *remoteRunner) Worker() domain.BuildWorker { return runner.worker }

func (runner *remoteRunner) OutputDigest() string {
	runner.digestMu.Lock()
	defer runner.digestMu.Unlock()
	return runner.digest
}

func (runner *remoteRunner) Run(ctx context.Context, workspace string, recipe domain.BuildRecipe) (string, error) {
	buildID := filepath.Base(workspace)
	if buildID == "." || buildID == string(filepath.Separator) || !safeWorkerValue.MatchString(buildID) {
		return "", ErrInvalidRequest
	}
	result := make(chan remoteResult, 1)
	runner.pool.mu.Lock()
	if _, duplicate := runner.pool.pending[buildID]; duplicate {
		runner.pool.mu.Unlock()
		return "", errors.New("remote build is already pending")
	}
	runner.pool.pending[buildID] = remotePending{nodeID: runner.nodeID, result: result}
	runner.pool.mu.Unlock()
	cleanup := func() {
		runner.pool.mu.Lock()
		delete(runner.pool.pending, buildID)
		runner.pool.mu.Unlock()
	}
	messageIDBytes := make([]byte, 12)
	if _, err := rand.Read(messageIDBytes); err != nil {
		cleanup()
		return "", err
	}
	if err := runner.pool.commands.Enqueue(runner.nodeID, &xirangv1.ControlMessage{
		MessageId: "build_command_" + hex.EncodeToString(messageIDBytes), SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RunNativeBuild{RunNativeBuild: &xirangv1.RunNativeBuild{
			BuildId: buildID, Workspace: workspace, Command: append([]string(nil), recipe.Command...), Steps: protoBuildSteps(recipe),
			OutputPath: recipe.OutputPath, Toolchain: recipe.Toolchain, Environment: recipe.Environment,
			MilliCpu: recipe.MilliCPU, MemoryBytes: recipe.MemoryBytes,
			Timeout: durationpb.New(recipe.Timeout), Architecture: runner.worker.Architecture,
			WorkspaceSha256: recipe.SourceSHA256,
		}},
	}); err != nil {
		cleanup()
		return "", err
	}
	select {
	case completed := <-result:
		runner.digestMu.Lock()
		runner.digest = completed.digest
		runner.digestMu.Unlock()
		return completed.logs, completed.err
	case <-ctx.Done():
		messageIDBytes := make([]byte, 12)
		if _, err := rand.Read(messageIDBytes); err == nil {
			_ = runner.pool.commands.Enqueue(runner.nodeID, &xirangv1.ControlMessage{
				MessageId: "build_cancel_" + hex.EncodeToString(messageIDBytes), SentAt: timestamppb.Now(),
				Payload: &xirangv1.ControlMessage_CancelNativeBuild{CancelNativeBuild: &xirangv1.CancelNativeBuild{BuildId: buildID, Reason: "controller canceled native build"}},
			})
		}
		cleanup()
		return "", ctx.Err()
	}
}

func protoBuildSteps(recipe domain.BuildRecipe) []*xirangv1.NativeBuildStep {
	if len(recipe.Steps) == 0 {
		return nil
	}
	result := make([]*xirangv1.NativeBuildStep, 0, len(recipe.Steps))
	for _, step := range recipe.Steps {
		result = append(result, &xirangv1.NativeBuildStep{Arguments: append([]string(nil), step...)})
	}
	return result
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

var _ RunnerRegistry = (*RemotePool)(nil)
var _ Runner = (*remoteRunner)(nil)
