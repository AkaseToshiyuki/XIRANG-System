package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestExecutorRequiresTrustedArtifactSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("artifact"))
	command := &xirangv1.RunAssignment{
		AssignmentId: "signed", Attempt: 1,
		Artifact: &xirangv1.NativeArtifact{
			Uri: "file:///tmp/artifact", Sha256: hex.EncodeToString(digest[:]),
			Architecture: runtime.GOARCH, Entrypoint: "artifact",
		},
	}
	executor := &NativeExecutor{allowRoot: true}
	executor.SetArtifactTrustKeys([]ed25519.PublicKey{publicKey})
	if err := executor.validateRunAssignment(command); err == nil {
		t.Fatal("unsigned artifact was accepted")
	}
	command.Artifact.Signature = ed25519.Sign(privateKey, digest[:])
	if err := executor.validateRunAssignment(command); err != nil {
		t.Fatalf("trusted artifact was rejected: %v", err)
	}
	command.Artifact.Signature[0] ^= 0xff
	if err := executor.validateRunAssignment(command); err == nil {
		t.Fatal("tampered artifact signature was accepted")
	}
}

func TestExecutorRechecksRuntimeContract(t *testing.T) {
	digest := sha256.Sum256([]byte("artifact"))
	command := &xirangv1.RunAssignment{AssignmentId: "runtime", Attempt: 1, Artifact: &xirangv1.NativeArtifact{
		Uri: "file:///tmp/artifact", Sha256: hex.EncodeToString(digest[:]), Architecture: runtime.GOARCH, Entrypoint: "artifact",
		RuntimeRequirements: map[string]string{"os": "linux", "libc.version": ">=2.17"},
	}}
	executor := &NativeExecutor{allowRoot: true, runtimeFacts: map[string]string{"os": "linux", "libc.version": "2.39"}}
	if err := executor.validateRunAssignment(command); err != nil {
		t.Fatalf("satisfied runtime contract was rejected: %v", err)
	}
	command.Artifact.RuntimeRequirements["libc.version"] = ">=2.40"
	if err := executor.validateRunAssignment(command); err == nil {
		t.Fatal("incompatible runtime contract was accepted")
	}
}

func TestArtifactCacheSurvivesSourceRemoval(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.bin")
	payload := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(source, payload, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	cache := filepath.Join(directory, "cache", hex.EncodeToString(digest[:]))
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	uri := "file://" + source
	if err := ensureCachedArtifact(context.Background(), uri, hex.EncodeToString(digest[:]), cache); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := ensureCachedArtifact(context.Background(), uri, hex.EncodeToString(digest[:]), cache); err != nil {
		t.Fatalf("cache miss after source removal: %v", err)
	}
}

func TestPublicArtifactAddressRejectsNonPublicRanges(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "192.0.2.1",
		"198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1", "::1",
		"fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::1",
	} {
		if publicArtifactAddress(address) {
			t.Errorf("non-public artifact address %q was accepted", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicArtifactAddress(address) {
			t.Errorf("public artifact address %q was rejected", address)
		}
	}
}

func TestWorkspaceStorageMeasurementDetectsExceededQuota(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "output"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	used, err := assignmentWorkspaceSize(root)
	if err != nil {
		t.Fatal(err)
	}
	if used <= 1024 {
		t.Fatalf("workspace usage = %d, expected quota exceedance", used)
	}
}

func TestRequestedGPUDevicesRecognizesVendorAndDRMResources(t *testing.T) {
	resources := &xirangv1.ResourceRequest{Devices: map[string]int64{
		"nvidia.com/gpu":       2,
		"xirang.io/drm-render": 1,
		"example.com/fpga":     4,
	}}
	if count := requestedGPUDevices(resources); count != 3 {
		t.Fatalf("requested GPU count = %d, want 3", count)
	}
}

func TestNativeExecutorSuspendsAndResumesForAdministratorStoragePolicy(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\nwhile :; do sleep 1; done\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	command := executorRunCommand("storage-policy", artifact)
	command.StorageQuotaBytes = 1
	if accepted, reason := executor.AcceptRun(context.Background(), command); !accepted {
		t.Fatalf("run rejected: %s", reason)
	}
	update := waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_SUSPENDED)
	if !strings.Contains(update.Reason, "storage quota exceeded") {
		t.Fatalf("unexpected suspension reason: %q", update.Reason)
	}
	if accepted, reason := executor.AcceptStoragePolicy(&xirangv1.SetAssignmentStoragePolicy{
		AssignmentId: command.AssignmentId, QuotaBytes: 1 << 30,
	}); !accepted {
		t.Fatalf("policy update rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING)
	if accepted, reason := executor.AcceptCancel(&xirangv1.CancelAssignment{AssignmentId: command.AssignmentId}); !accepted {
		t.Fatalf("cancel rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED)
}

func TestNativeExecutorPersistsBoundedLogTails(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\ni=0\nwhile [ \"$i\" -lt 70000 ]; do printf x; i=$((i + 1)); done\nprintf '\\nstdout-end\\n'\nprintf 'stderr-end\\n' >&2\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("assignment-logs", artifact)
	accepted, reason := executor.AcceptRun(context.Background(), command)
	if !accepted {
		t.Fatalf("assignment rejected: %s", reason)
	}
	update := waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED)
	if len(update.StdoutTail) != maxAssignmentLogBytes || !bytes.HasSuffix(update.StdoutTail, []byte("stdout-end\n")) || string(update.StderrTail) != "stderr-end\n" || !update.LogsTruncated {
		t.Fatalf("unexpected bounded logs: stdout=%d suffix=%q stderr=%q truncated=%t", len(update.StdoutTail), update.StdoutTail[len(update.StdoutTail)-16:], update.StderrTail, update.LogsTruncated)
	}
	reopened, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := reopened.Snapshots()
	if len(snapshots) != 1 || !bytes.Equal(snapshots[0].StdoutTail, update.StdoutTail) || string(snapshots[0].StderrTail) != "stderr-end\n" || !snapshots[0].LogsTruncated {
		t.Fatalf("logs did not survive Agent restart: %#v", snapshots)
	}
}

func TestNativeExecutorPublishesLiveLogSnapshots(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\nprintf 'live-stdout\\n'\nprintf 'live-stderr\\n' >&2\nsleep 30\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("assignment-live-logs", artifact)
	accepted, reason := executor.AcceptRun(context.Background(), command)
	if !accepted {
		t.Fatalf("assignment rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING)
	select {
	case snapshot := <-executor.Logs():
		if string(snapshot.StdoutTail) != "live-stdout\n" || string(snapshot.StderrTail) != "live-stderr\n" {
			t.Fatalf("unexpected live log snapshot: %#v", snapshot)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for live log snapshot")
	}
	if accepted, reason := executor.AcceptCancel(&xirangv1.CancelAssignment{AssignmentId: command.AssignmentId, GracePeriod: durationpb.New(0)}); !accepted {
		t.Fatalf("cancel rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED)
}

func TestNativeExecutorTimeoutKillsProcessAndPreservesLogs(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\nprintf 'before-timeout\\n'\nsleep 30\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("assignment-timeout", artifact)
	command.Timeout = durationpb.New(100 * time.Millisecond)
	accepted, reason := executor.AcceptRun(context.Background(), command)
	if !accepted {
		t.Fatalf("assignment rejected: %s", reason)
	}
	update := waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED)
	if update.Reason != "assignment timed out" || string(update.StdoutTail) != "before-timeout\n" {
		t.Fatalf("unexpected timeout result: %#v", update)
	}
}

func TestNativeExecutorPersistsAndDeduplicatesAssignment(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\nprintf 'executed\\n' >> result.txt\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("assignment-1", artifact)
	accepted, reason := executor.AcceptRun(context.Background(), command)
	if !accepted {
		t.Fatalf("assignment rejected: %s", reason)
	}
	update := waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED)
	if update.AssignmentId != command.AssignmentId || update.Attempt != command.Attempt {
		t.Fatalf("unexpected terminal update: %#v", update)
	}
	accepted, reason = executor.AcceptRun(context.Background(), command)
	if !accepted {
		t.Fatalf("idempotent replay rejected: %s", reason)
	}
	time.Sleep(50 * time.Millisecond)
	resultPath := filepath.Join(directory, "assignments", workspaceName(command.AssignmentId, command.Attempt), "result.txt")
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "executed\n" {
		t.Fatalf("assignment ran more than once: %q", result)
	}
	reopened, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := reopened.Snapshots()
	if len(snapshots) != 1 || snapshots[0].State != xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED {
		t.Fatalf("terminal state did not persist: %#v", snapshots)
	}
	info, err := os.Stat(filepath.Join(directory, assignmentStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected assignment-state permissions: %o", info.Mode().Perm())
	}
}

func TestNativeExecutorGangBarrierAndLocalRanks(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\nprintf '%s/%s/%s\\n' \"$XIRANG_RANK\" \"$XIRANG_WORLD_SIZE\" \"$XIRANG_PRIMARY_ADDRESS\" > \"rank-$XIRANG_LOCAL_RANK\"\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("gang-member-1", artifact)
	command.AllocationId = "allocation-1"
	command.NodeRank, command.RankStart, command.RankCount, command.WorldSize = 0, 0, 2, 4
	command.Launcher, command.StartBarrier = "native", true
	command.NetworkInterface, command.PrimaryAddress = "eth0", "10.0.0.1"
	command.Hosts = []*xirangv1.AllocationHost{
		{NodeId: "node-1", Hostname: "node-1", Address: "10.0.0.1", NetworkInterface: "eth0", Slots: 2},
		{NodeId: "node-2", Hostname: "node-2", Address: "10.0.0.2", NetworkInterface: "eth0", Slots: 2},
	}
	if accepted, reason := executor.AcceptRun(context.Background(), command); !accepted {
		t.Fatalf("gang prepare rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_READY)
	workspace := filepath.Join(directory, "assignments", workspaceName(command.AssignmentId, command.Attempt))
	if _, err := os.Stat(filepath.Join(workspace, "rank-0")); !os.IsNotExist(err) {
		t.Fatalf("rank started before release: %v", err)
	}
	if accepted, reason := executor.AcceptRelease(&xirangv1.ReleaseAssignment{AssignmentId: command.AssignmentId, AllocationId: command.AllocationId}); !accepted {
		t.Fatalf("gang release rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED)
	for localRank, expected := range []string{"0/4/10.0.0.1\n", "1/4/10.0.0.1\n"} {
		data, err := os.ReadFile(filepath.Join(workspace, fmt.Sprintf("rank-%d", localRank)))
		if err != nil || string(data) != expected {
			t.Fatalf("local rank %d output = %q, %v", localRank, data, err)
		}
	}
	hostfile, err := os.ReadFile(filepath.Join(workspace, ".xirang", "hosts"))
	if err != nil || string(hostfile) != "10.0.0.1 slots=2\n10.0.0.2 slots=2\n" {
		t.Fatalf("unexpected hostfile %q: %v", hostfile, err)
	}
	manifest, err := os.ReadFile(filepath.Join(workspace, ".xirang", "allocation.json"))
	if err != nil || !strings.Contains(string(manifest), `"version":"xirang-allocation-v1"`) ||
		!strings.Contains(string(manifest), `"allocationId":"allocation-1"`) || strings.Contains(string(manifest), "environment") {
		t.Fatalf("unexpected bounded allocation manifest %q: %v", manifest, err)
	}
}

func TestNativeExecutorCanCancelPreparedGangMember(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\nexit 0\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("gang-cancel", artifact)
	command.AllocationId, command.RankCount, command.WorldSize, command.Launcher, command.StartBarrier = "allocation-cancel", 1, 2, "native", true
	command.Hosts = []*xirangv1.AllocationHost{
		{NodeId: "node-1", Hostname: "node-1", Address: "10.0.0.1", Slots: 1},
		{NodeId: "node-2", Hostname: "node-2", Address: "10.0.0.2", Slots: 1},
	}
	if accepted, reason := executor.AcceptRun(context.Background(), command); !accepted {
		t.Fatalf("gang prepare rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_READY)
	if accepted, reason := executor.AcceptCancel(&xirangv1.CancelAssignment{AssignmentId: command.AssignmentId}); !accepted {
		t.Fatalf("prepared cancellation rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED)
}

func TestCertifiedMPIAdapterCapabilityAndInvocation(t *testing.T) {
	bin := t.TempDir()
	adapter := filepath.Join(bin, mpiAdapterName("openmpi"))
	if err := os.WriteFile(adapter, []byte("#!/bin/sh\nif [ \"$1\" = \"--xirang-probe\" ]; then printf 'xirang-mpi-adapter-v1 openmpi\\n'; exit 0; fi\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	capabilities := detectedMPILauncherCapabilities()
	if len(capabilities) != 1 || capabilities[0] != "mpi-launch-v1/openmpi" {
		t.Fatalf("only the self-tested adapter should be advertised: %#v", capabilities)
	}
	command := &xirangv1.RunAssignment{Launcher: "openmpi"}
	executable, arguments, err := mpiAdapterInvocation(command, "/workspace", "/workspace/task", []string{"input.dat"}, 3)
	if err != nil || executable != adapter {
		t.Fatalf("adapter invocation was not resolved: executable=%q err=%v", executable, err)
	}
	want := []string{"--xirang-manifest", "/workspace/.xirang/allocation.json", "--xirang-local-rank", "3", "--", "/workspace/task", "input.dat"}
	if fmt.Sprint(arguments) != fmt.Sprint(want) {
		t.Fatalf("adapter arguments = %#v, want %#v", arguments, want)
	}
	command.Launcher = "mpich"
	if _, _, err := mpiAdapterInvocation(command, "/workspace", "/workspace/task", nil, 0); err == nil {
		t.Fatal("an uninstalled MPI adapter was accepted")
	}
}

func TestNativeExecutorRejectsChangedReplayAndCancelsProcessGroup(t *testing.T) {
	directory := t.TempDir()
	artifact := writeExecutorArtifact(t, directory, "#!/bin/sh\nsleep 30\n")
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("assignment-cancel", artifact)
	accepted, reason := executor.AcceptRun(context.Background(), command)
	if !accepted {
		t.Fatalf("assignment rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING)
	changed := executorRunCommand(command.AssignmentId, artifact)
	changed.Arguments = []string{"different"}
	if accepted, _ := executor.AcceptRun(context.Background(), changed); accepted {
		t.Fatal("changed assignment replay was accepted")
	}
	accepted, reason = executor.AcceptCancel(&xirangv1.CancelAssignment{
		AssignmentId: command.AssignmentId, GracePeriod: durationpb.New(0),
	})
	if !accepted {
		t.Fatalf("cancel rejected: %s", reason)
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED)
}

func TestNativeExecutorRejectsUnsafeOrUnenforcedAssignment(t *testing.T) {
	executor, err := OpenNativeExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	command := executorRunCommand("assignment-invalid", "/missing")
	command.Artifact.Entrypoint = "../escape"
	if accepted, _ := executor.AcceptRun(context.Background(), command); accepted {
		t.Fatal("path traversal entrypoint was accepted")
	}
	command = executorRunCommand("assignment-limits", "/missing")
	command.Resources = &xirangv1.ResourceRequest{MilliCpu: 1000}
	if accepted, _ := executor.AcceptRun(context.Background(), command); accepted {
		t.Fatal("unenforced resource limit was accepted")
	}
}

func writeExecutorArtifact(t *testing.T, directory, contents string) string {
	t.Helper()
	path := filepath.Join(directory, "artifact.sh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func executorRunCommand(id, artifactPath string) *xirangv1.RunAssignment {
	data, _ := os.ReadFile(artifactPath)
	digest := sha256.Sum256(data)
	return &xirangv1.RunAssignment{
		AssignmentId: id,
		JobId:        "job-1",
		Attempt:      1,
		Artifact: &xirangv1.NativeArtifact{
			Uri:          "file://" + artifactPath,
			Sha256:       fmt.Sprintf("%x", digest),
			Architecture: runtime.GOARCH,
			Entrypoint:   "task.sh",
		},
		Timeout: durationpb.New(time.Minute),
	}
}

func waitForAssignmentState(t *testing.T, updates <-chan *xirangv1.AssignmentUpdate, expected xirangv1.AssignmentState) *xirangv1.AssignmentUpdate {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case update := <-updates:
			if update.State == expected {
				return update
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for assignment state %s", expected)
		}
	}
}
