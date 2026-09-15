package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTelemetryIntervalCommandValidatesAndApplies(t *testing.T) {
	var applied time.Duration
	message := &xirangv1.ControlMessage{
		MessageId: "telemetry-interval", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_SetTelemetryInterval{SetTelemetryInterval: &xirangv1.SetTelemetryInterval{
			Interval: durationpb.New(7 * time.Second),
		}},
	}
	_, acknowledgement, err := applyControlCommandWithHandlers(context.Background(), t.TempDir(), commandState{Mode: "ready"}, message, commandHandlers{
		setTelemetryInterval: func(interval time.Duration) (bool, string) {
			applied = interval
			return true, ""
		},
	})
	if err != nil || !acknowledgement.Accepted || applied != 7*time.Second {
		t.Fatalf("unexpected interval command applied=%s acknowledgement=%#v err=%v", applied, acknowledgement, err)
	}
}

func TestUpdateNetworkProbeTargetsCommand(t *testing.T) {
	message := &xirangv1.ControlMessage{
		MessageId: "network-targets-1", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_UpdateNetworkProbeTargets{
			UpdateNetworkProbeTargets: &xirangv1.UpdateNetworkProbeTargets{Targets: []*xirangv1.NetworkProbeTarget{
				{NodeId: "node-b", InterfaceName: "eth0", Address: "192.0.2.2", SpeedBitsPerSecond: 25_000_000_000},
			}},
		},
	}
	var received []*xirangv1.NetworkProbeTarget
	_, acknowledgement, err := applyControlCommandWithHandlers(context.Background(), t.TempDir(), commandState{Mode: "ready"}, message, commandHandlers{
		setNetworkTargets: func(targets []*xirangv1.NetworkProbeTarget) (bool, string) {
			received = targets
			return true, ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !acknowledgement.Accepted || len(received) != 1 || received[0].Address != "192.0.2.2" {
		t.Fatalf("network target refresh was not applied: ack=%#v targets=%#v", acknowledgement, received)
	}
}

func TestManagementPanelCommandPersistsOnlyAcceptedState(t *testing.T) {
	directory := t.TempDir()
	message := &xirangv1.ControlMessage{
		MessageId: "management-panel-enable", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_SetManagementPanel{SetManagementPanel: &xirangv1.SetManagementPanel{Enabled: true}},
	}
	updated, acknowledgement, err := applyControlCommandWithHandlers(context.Background(), directory, commandState{Mode: "ready"}, message, commandHandlers{
		setManagementPanel: func(enabled bool) (bool, string) { return enabled, "" },
	})
	if err != nil || !acknowledgement.Accepted || !updated.ManagementPanelEnabled {
		t.Fatalf("management panel enable was not persisted: state=%#v ack=%#v err=%v", updated, acknowledgement, err)
	}
	reloaded, err := loadCommandState(directory)
	if err != nil || !reloaded.ManagementPanelEnabled {
		t.Fatalf("management panel state was not restored: state=%#v err=%v", reloaded, err)
	}

	rejected := &xirangv1.ControlMessage{
		MessageId: "management-panel-disable", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_SetManagementPanel{SetManagementPanel: &xirangv1.SetManagementPanel{Enabled: false}},
	}
	unchanged, rejection, err := applyControlCommandWithHandlers(context.Background(), directory, updated, rejected, commandHandlers{
		setManagementPanel: func(bool) (bool, string) { return false, "external service owns port 8899" },
	})
	if err != nil || rejection.Accepted || !unchanged.ManagementPanelEnabled {
		t.Fatalf("rejected panel transition changed durable state: state=%#v ack=%#v err=%v", unchanged, rejection, err)
	}
}

func TestCommandStatePersistsAndDeduplicatesNodeMode(t *testing.T) {
	directory := t.TempDir()
	state, err := loadCommandState(directory)
	if err != nil {
		t.Fatal(err)
	}
	command := agentTestModeCommand("command-1", xirangv1.NodeMode_NODE_MODE_CORDONED)
	updated, acknowledgement, err := applyControlCommand(directory, state, command)
	if err != nil {
		t.Fatal(err)
	}
	if !acknowledgement.Accepted || commandStateMode(updated) != xirangv1.NodeMode_NODE_MODE_CORDONED {
		t.Fatalf("unexpected command result: state=%#v acknowledgement=%#v", updated, acknowledgement)
	}

	duplicate := agentTestModeCommand("command-1", xirangv1.NodeMode_NODE_MODE_READY)
	deduplicated, repeated, err := applyControlCommand(directory, updated, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.Accepted || commandStateMode(deduplicated) != xirangv1.NodeMode_NODE_MODE_CORDONED || len(deduplicated.Acknowledgements) != 1 {
		t.Fatalf("duplicate command changed state: %#v %#v", deduplicated, repeated)
	}

	reloaded, err := loadCommandState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if commandStateMode(reloaded) != xirangv1.NodeMode_NODE_MODE_CORDONED {
		t.Fatalf("node mode did not persist: %#v", reloaded)
	}
	info, err := os.Stat(filepath.Join(directory, commandStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected command-state permissions: %o", info.Mode().Perm())
	}
}

func TestCommandStateRejectsUnsupportedCommand(t *testing.T) {
	directory := t.TempDir()
	state := commandState{Mode: "ready"}
	message := &xirangv1.ControlMessage{
		MessageId: "command-unsupported", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RotateCertificate{RotateCertificate: &xirangv1.RotateCertificate{RotationId: "rotation-1"}},
	}
	updated, acknowledgement, err := applyControlCommand(directory, state, message)
	if err != nil {
		t.Fatal(err)
	}
	if acknowledgement.Accepted || acknowledgement.Reason == "" || commandStateMode(updated) != xirangv1.NodeMode_NODE_MODE_READY {
		t.Fatalf("unexpected unsupported-command result: %#v %#v", updated, acknowledgement)
	}
}

func TestCommandStateRotatesCertificateOnceAndRecoversCrashWindow(t *testing.T) {
	directory := t.TempDir()
	identity := "serial-old"
	rotations := 0
	handlers := commandHandlers{
		currentIdentity: func() string { return identity },
		rotateCertificate: func(context.Context) (string, error) {
			rotations++
			identity = "serial-new"
			return identity, nil
		},
	}
	message := &xirangv1.ControlMessage{
		MessageId: "command-rotate", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RotateCertificate{RotateCertificate: &xirangv1.RotateCertificate{RotationId: "rotation-1"}},
	}
	updated, acknowledgement, err := applyControlCommandWithHandlers(context.Background(), directory, commandState{Mode: "ready"}, message, handlers)
	if err != nil {
		t.Fatal(err)
	}
	if !acknowledgement.Accepted || rotations != 1 || updated.PendingCertificateRotation != nil {
		t.Fatalf("unexpected certificate rotation result: state=%#v acknowledgement=%#v rotations=%d", updated, acknowledgement, rotations)
	}
	deduplicated, repeated, err := applyControlCommandWithHandlers(context.Background(), directory, updated, message, handlers)
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.Accepted || rotations != 1 || len(deduplicated.Acknowledgements) != 1 {
		t.Fatalf("certificate rotation replay repeated its side effect: state=%#v acknowledgement=%#v rotations=%d", deduplicated, repeated, rotations)
	}

	crashDirectory := t.TempDir()
	crashState := commandState{Mode: "ready", PendingCertificateRotation: &pendingCertificateRotation{
		MessageID: "command-crash", RotationID: "rotation-crash", PreviousIdentity: "serial-before-crash",
	}}
	if err := persistCommandState(crashDirectory, crashState); err != nil {
		t.Fatal(err)
	}
	crashCalls := 0
	crashMessage := &xirangv1.ControlMessage{
		MessageId: "command-crash", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RotateCertificate{RotateCertificate: &xirangv1.RotateCertificate{RotationId: "rotation-crash"}},
	}
	recovered, recoveredAck, err := applyControlCommandWithHandlers(context.Background(), crashDirectory, crashState, crashMessage, commandHandlers{
		currentIdentity: func() string { return "serial-after-crash" },
		rotateCertificate: func(context.Context) (string, error) {
			crashCalls++
			return "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recoveredAck.Accepted || crashCalls != 0 || recovered.PendingCertificateRotation != nil {
		t.Fatalf("crash recovery repeated certificate rotation: state=%#v acknowledgement=%#v calls=%d", recovered, recoveredAck, crashCalls)
	}
}

func TestCommandStateAcceptsNativeAssignmentAfterDurableRegistration(t *testing.T) {
	directory := t.TempDir()
	artifact := filepath.Join(directory, "task.sh")
	contents := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(artifact, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	executor.allowRoot = true
	message := &xirangv1.ControlMessage{
		MessageId: "command-assignment", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RunAssignment{RunAssignment: &xirangv1.RunAssignment{
			AssignmentId: "assignment-1", JobId: "job-1", Attempt: 1,
			Artifact: &xirangv1.NativeArtifact{
				Uri: "file://" + artifact, Sha256: fmt.Sprintf("%x", digest),
				Architecture: runtime.GOARCH, Entrypoint: "task.sh",
			},
		}},
	}
	updated, acknowledgement, err := applyControlCommandContext(context.Background(), directory, commandState{Mode: "ready"}, message, executor)
	if err != nil {
		t.Fatal(err)
	}
	if !acknowledgement.Accepted || len(updated.Acknowledgements) != 1 || len(executor.Snapshots()) != 1 {
		t.Fatalf("assignment command was not durably accepted: state=%#v acknowledgement=%#v snapshots=%#v", updated, acknowledgement, executor.Snapshots())
	}
	waitForAssignmentState(t, executor.Updates(), xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED)
}

func agentTestModeCommand(id string, mode xirangv1.NodeMode) *xirangv1.ControlMessage {
	return &xirangv1.ControlMessage{
		MessageId: id, SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_SetNodeMode{SetNodeMode: &xirangv1.SetNodeMode{Mode: mode}},
	}
}
