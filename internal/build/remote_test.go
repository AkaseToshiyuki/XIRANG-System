package build

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type remoteTestQueue struct {
	mu      sync.Mutex
	active  bool
	nodeID  string
	message *xirangv1.ControlMessage
	ready   chan struct{}
}

func (queue *remoteTestQueue) Enqueue(nodeID string, message *xirangv1.ControlMessage) error {
	queue.mu.Lock()
	queue.nodeID, queue.message = nodeID, message
	queue.mu.Unlock()
	select {
	case queue.ready <- struct{}{}:
	default:
	}
	return nil
}

func (queue *remoteTestQueue) NodeActive(string) bool { return queue.active }

func TestRemotePoolSelectsArchitectureAndCompletesBuild(t *testing.T) {
	queue := &remoteTestQueue{active: true, ready: make(chan struct{}, 1)}
	pool := NewRemotePool(queue)
	if err := pool.ReportWorker("node-arm", &xirangv1.NativeBuildWorker{
		Architecture: "arm64", Toolchains: []string{"go", "cc"}, Isolation: "dedicated-uid+cgroup-v2", Ready: true,
	}); err != nil {
		t.Fatal(err)
	}
	runner, ok := pool.Select(domain.BuildRecipe{Architecture: "arm64", Toolchain: "go"})
	if !ok || runner.Worker().ID != "agent:node-arm" {
		t.Fatalf("unexpected remote runner: %#v %t", runner, ok)
	}
	type outcome struct {
		logs string
		err  error
	}
	completed := make(chan outcome, 1)
	go func() {
		logs, err := runner.Run(context.Background(), "/shared/build-work/build_0123456789abcdef01234567", domain.BuildRecipe{
			Architecture: "arm64", Toolchain: "go", Command: []string{"go", "build"}, OutputPath: "bin/app",
			MilliCPU: 1000, MemoryBytes: 1 << 30, Timeout: time.Minute,
		})
		completed <- outcome{logs: logs, err: err}
	}()
	select {
	case <-queue.ready:
	case <-time.After(time.Second):
		t.Fatal("remote command was not queued")
	}
	queue.mu.Lock()
	message := queue.message
	nodeID := queue.nodeID
	queue.mu.Unlock()
	command := message.GetRunNativeBuild()
	if nodeID != "node-arm" || command == nil || command.Architecture != "arm64" || command.Workspace != "/shared/build-work/build_0123456789abcdef01234567" {
		t.Fatalf("unexpected command: node=%q command=%#v", nodeID, command)
	}
	if err := pool.ReportUpdate("node-arm", &xirangv1.NativeBuildUpdate{
		BuildId: command.BuildId, State: xirangv1.NativeBuildState_NATIVE_BUILD_STATE_SUCCEEDED,
		LogTail: "compiled", OutputSha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", OccurredAt: timestamppb.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-completed:
		if result.err != nil || result.logs != "compiled" {
			t.Fatalf("unexpected outcome: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("remote build did not complete")
	}
}

func TestRemotePoolSelectsBuilderArchitectureAndPinnedWorker(t *testing.T) {
	queue := &remoteTestQueue{active: true, ready: make(chan struct{}, 1)}
	pool := NewRemotePool(queue)
	capability := []*xirangv1.ToolchainCapability{{Name: "go", TargetArchitectures: []string{"amd64", "arm64"}, Executables: []string{"go"}}}
	_ = pool.ReportWorker("node-arm-a", &xirangv1.NativeBuildWorker{Architecture: "arm64", Toolchains: []string{"go"}, ToolchainCapabilities: capability, Ready: true})
	_ = pool.ReportWorker("node-arm-b", &xirangv1.NativeBuildWorker{Architecture: "arm64", Toolchains: []string{"go"}, ToolchainCapabilities: capability, Ready: true})
	runner, ok := pool.Select(domain.BuildRecipe{Architecture: "amd64", BuilderArchitecture: "arm64", CrossCompile: true, WorkerID: "agent:node-arm-b", Toolchain: "go"})
	if !ok || runner.Worker().ID != "agent:node-arm-b" {
		t.Fatalf("pinned cross-compile worker = %#v ok=%t", runner, ok)
	}
}

func TestRemotePoolFailsPendingBuildOnDisconnect(t *testing.T) {
	queue := &remoteTestQueue{active: true, ready: make(chan struct{}, 1)}
	pool := NewRemotePool(queue)
	_ = pool.ReportWorker("node-a", &xirangv1.NativeBuildWorker{
		Architecture: "amd64", Toolchains: []string{"cc"}, Isolation: "cgroup-v2", Ready: true,
	})
	runner, _ := pool.Select(domain.BuildRecipe{Architecture: "amd64", Toolchain: "cc"})
	completed := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), "/shared/build-work/build_abcdef0123456789abcdef01", domain.BuildRecipe{
			Architecture: "amd64", Toolchain: "cc", Command: []string{"cc", "main.c"}, OutputPath: "app",
			MilliCPU: 1000, MemoryBytes: 1 << 20, Timeout: time.Minute,
		})
		completed <- err
	}()
	<-queue.ready
	pool.RemoveWorker("node-a")
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("disconnect did not fail the pending build")
		}
	case <-time.After(time.Second):
		t.Fatal("pending build remained blocked")
	}
}

func TestRemotePoolDispatchesCancellation(t *testing.T) {
	queue := &remoteTestQueue{active: true, ready: make(chan struct{}, 2)}
	pool := NewRemotePool(queue)
	_ = pool.ReportWorker("node-a", &xirangv1.NativeBuildWorker{Architecture: "amd64", Toolchains: []string{"go"}, Ready: true})
	runner, ok := pool.Select(domain.BuildRecipe{Architecture: "amd64", Toolchain: "go"})
	if !ok {
		t.Fatal("native worker was not selected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, "/shared/build-work/build_abcdef0123456789abcdef01", domain.BuildRecipe{Architecture: "amd64", Toolchain: "go", Command: []string{"go", "build"}, OutputPath: "app", MilliCPU: 1000, MemoryBytes: 1 << 20, Timeout: time.Minute})
		completed <- err
	}()
	<-queue.ready
	cancel()
	<-queue.ready
	queue.mu.Lock()
	message := queue.message
	queue.mu.Unlock()
	if message.GetCancelNativeBuild() == nil {
		t.Fatalf("cancellation command was not dispatched: %#v", message)
	}
	if err := <-completed; !errors.Is(err, context.Canceled) {
		t.Fatalf("remote runner cancellation error = %v", err)
	}
}
