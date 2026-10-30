package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
)

const (
	commandStateFile  = "command-state.json"
	commandLedgerSize = 1024
)

type commandState struct {
	Mode                       string                      `json:"mode"`
	ManagementPanelEnabled     bool                        `json:"managementPanelEnabled,omitempty"`
	Acknowledgements           []commandAcknowledgment     `json:"acknowledgements"`
	PendingCertificateRotation *pendingCertificateRotation `json:"pendingCertificateRotation,omitempty"`
}

type commandAcknowledgment struct {
	MessageID string `json:"messageId"`
	Accepted  bool   `json:"accepted"`
	Reason    string `json:"reason,omitempty"`
}

type pendingCertificateRotation struct {
	MessageID        string `json:"messageId"`
	RotationID       string `json:"rotationId"`
	PreviousIdentity string `json:"previousIdentity"`
}

type commandHandlers struct {
	setBMCIdentify       func(context.Context, *xirangv1.SetBMCIdentify) (bool, string)
	executor             *NativeExecutor
	currentIdentity      func() string
	rotateCertificate    func(context.Context) (string, error)
	setTelemetryInterval func(time.Duration) (bool, string)
	setNetworkTargets    func([]*xirangv1.NetworkProbeTarget) (bool, string)
	setManagementPanel   func(bool) (bool, string)
	terminal             *TerminalManager
	nativeBuild          *NativeBuildManager
	mounts               *sharedMountManager
}

func loadCommandState(directory string) (commandState, error) {
	data, err := os.ReadFile(filepath.Join(directory, commandStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return commandState{Mode: "ready"}, nil
	}
	if err != nil {
		return commandState{}, err
	}
	var state commandState
	if err := json.Unmarshal(data, &state); err != nil {
		return commandState{}, fmt.Errorf("decode Agent command state: %w", err)
	}
	if _, ok := nodeModeFromText(state.Mode); !ok {
		return commandState{}, errors.New("the Agent command state has an invalid node mode")
	}
	if len(state.Acknowledgements) > commandLedgerSize {
		state.Acknowledgements = state.Acknowledgements[len(state.Acknowledgements)-commandLedgerSize:]
	}
	if pending := state.PendingCertificateRotation; pending != nil && (pending.MessageID == "" || pending.RotationID == "" || pending.PreviousIdentity == "") {
		return commandState{}, errors.New("the Agent command state has an invalid pending certificate rotation")
	}
	return state, nil
}

func applyControlCommand(directory string, state commandState, message *xirangv1.ControlMessage, executors ...*NativeExecutor) (commandState, *xirangv1.CommandAcknowledgement, error) {
	return applyControlCommandContext(context.Background(), directory, state, message, executors...)
}

func applyControlCommandContext(ctx context.Context, directory string, state commandState, message *xirangv1.ControlMessage, executors ...*NativeExecutor) (commandState, *xirangv1.CommandAcknowledgement, error) {
	var executor *NativeExecutor
	if len(executors) > 0 {
		executor = executors[0]
	}
	return applyControlCommandWithHandlers(ctx, directory, state, message, commandHandlers{executor: executor})
}

func applyControlCommandWithHandlers(ctx context.Context, directory string, state commandState, message *xirangv1.ControlMessage, handlers commandHandlers) (commandState, *xirangv1.CommandAcknowledgement, error) {
	if message == nil || message.MessageId == "" {
		return state, nil, errors.New("control command message ID is required")
	}
	if terminalCommand(message) {
		accepted, reason := applyTerminalCommand(message, handlers.terminal)
		return state, &xirangv1.CommandAcknowledgement{CommandMessageId: message.MessageId, Accepted: accepted, Reason: reason}, nil
	}
	if operation := message.GetSharedMountOperation(); operation != nil {
		if operation.OperationId != message.MessageId {
			return state, &xirangv1.CommandAcknowledgement{CommandMessageId: message.MessageId, Reason: "mount operation ID must match command ID"}, nil
		}
		accepted, reason := handlers.mounts.Start(operation)
		return state, &xirangv1.CommandAcknowledgement{CommandMessageId: message.MessageId, Accepted: accepted, Reason: reason}, nil
	}
	for _, recorded := range state.Acknowledgements {
		if recorded.MessageID == message.MessageId {
			return state, &xirangv1.CommandAcknowledgement{
				CommandMessageId: recorded.MessageID, Accepted: recorded.Accepted, Reason: recorded.Reason,
			}, nil
		}
	}

	acknowledgement := commandAcknowledgment{MessageID: message.MessageId}
	updated := state
	var managementPanelRollback *bool
	if command := message.GetSetNodeMode(); command != nil {
		mode, ok := nodeModeText(command.Mode)
		if ok {
			updated.Mode = mode
			acknowledgement.Accepted = true
		} else {
			acknowledgement.Reason = "unsupported node mode"
		}
	} else if command := message.GetRunAssignment(); command != nil && handlers.executor != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.executor.AcceptRun(ctx, command)
	} else if command := message.GetCancelAssignment(); command != nil && handlers.executor != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.executor.AcceptCancel(command)
	} else if command := message.GetReleaseAssignment(); command != nil && handlers.executor != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.executor.AcceptRelease(command)
	} else if command := message.GetSetAssignmentStoragePolicy(); command != nil && handlers.executor != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.executor.AcceptStoragePolicy(command)
	} else if command := message.GetRotateCertificate(); command != nil {
		var err error
		updated, acknowledgement.Accepted, acknowledgement.Reason, err = applyCertificateRotation(ctx, directory, updated, message.MessageId, command, handlers)
		if err != nil {
			return state, nil, err
		}
	} else if command := message.GetSetTelemetryInterval(); command != nil && handlers.setTelemetryInterval != nil {
		if command.Interval == nil {
			acknowledgement.Reason = "telemetry interval is required"
		} else {
			acknowledgement.Accepted, acknowledgement.Reason = handlers.setTelemetryInterval(command.Interval.AsDuration())
		}
	} else if command := message.GetUpdateNetworkProbeTargets(); command != nil && handlers.setNetworkTargets != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.setNetworkTargets(command.Targets)
	} else if command := message.GetSetBmcIdentify(); command != nil && handlers.setBMCIdentify != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.setBMCIdentify(ctx, command)
	} else if command := message.GetSetManagementPanel(); command != nil && handlers.setManagementPanel != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.setManagementPanel(command.Enabled)
		if acknowledgement.Accepted {
			previous := state.ManagementPanelEnabled
			managementPanelRollback = &previous
			updated.ManagementPanelEnabled = command.Enabled
		}
	} else if command := message.GetRunNativeBuild(); command != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.nativeBuild.Start(command)
	} else if command := message.GetCancelNativeBuild(); command != nil {
		acknowledgement.Accepted, acknowledgement.Reason = handlers.nativeBuild.Cancel(command)
	} else {
		acknowledgement.Reason = "unsupported control command"
	}
	updated.Acknowledgements = append(append([]commandAcknowledgment(nil), state.Acknowledgements...), acknowledgement)
	if len(updated.Acknowledgements) > commandLedgerSize {
		updated.Acknowledgements = updated.Acknowledgements[len(updated.Acknowledgements)-commandLedgerSize:]
	}
	if message.GetRotateCertificate() != nil {
		updated.PendingCertificateRotation = nil
	}
	if err := persistCommandState(directory, updated); err != nil {
		if managementPanelRollback != nil && handlers.setManagementPanel != nil {
			_, _ = handlers.setManagementPanel(*managementPanelRollback)
		}
		return state, nil, fmt.Errorf("persist Agent command acknowledgement: %w", err)
	}
	return updated, &xirangv1.CommandAcknowledgement{
		CommandMessageId: acknowledgement.MessageID,
		Accepted:         acknowledgement.Accepted,
		Reason:           acknowledgement.Reason,
	}, nil
}

func terminalCommand(message *xirangv1.ControlMessage) bool {
	return message.GetOpenTerminal() != nil || message.GetTerminalInput() != nil || message.GetResizeTerminal() != nil || message.GetCloseTerminal() != nil
}

func applyTerminalCommand(message *xirangv1.ControlMessage, manager *TerminalManager) (bool, string) {
	if manager == nil {
		return false, "Agent PTY is unavailable"
	}
	switch {
	case message.GetOpenTerminal() != nil:
		return manager.Open(message.GetOpenTerminal())
	case message.GetTerminalInput() != nil:
		return manager.Input(message.GetTerminalInput())
	case message.GetResizeTerminal() != nil:
		return manager.Resize(message.GetResizeTerminal())
	case message.GetCloseTerminal() != nil:
		return manager.Close(message.GetCloseTerminal())
	default:
		return false, "unsupported terminal command"
	}
}

func applyCertificateRotation(ctx context.Context, directory string, state commandState, messageID string, command *xirangv1.RotateCertificate, handlers commandHandlers) (commandState, bool, string, error) {
	if command.RotationId == "" {
		return state, false, "certificate rotation ID is required", nil
	}
	if len(command.CertificateChainPem) != 0 {
		return state, false, "controller-provided certificate chains are not accepted", nil
	}
	if handlers.currentIdentity == nil || handlers.rotateCertificate == nil || handlers.currentIdentity() == "" {
		return state, false, "certificate rotation handler is unavailable", nil
	}
	identity := handlers.currentIdentity()
	pending := state.PendingCertificateRotation
	if pending != nil && (pending.MessageID != messageID || pending.RotationID != command.RotationId) {
		return state, false, "another certificate rotation is already in progress", nil
	}
	if pending == nil {
		state.PendingCertificateRotation = &pendingCertificateRotation{
			MessageID: messageID, RotationID: command.RotationId, PreviousIdentity: identity,
		}
		if err := persistCommandState(directory, state); err != nil {
			return state, false, "", fmt.Errorf("persist pending certificate rotation: %w", err)
		}
		pending = state.PendingCertificateRotation
	}
	if identity != pending.PreviousIdentity {
		return state, true, "", nil
	}
	rotatedIdentity, err := handlers.rotateCertificate(ctx)
	if rotatedIdentity != "" && rotatedIdentity != pending.PreviousIdentity {
		return state, true, "", nil
	}
	if err != nil {
		return state, false, err.Error(), nil
	}
	return state, false, "certificate rotation did not activate a new identity", nil
}

func persistCommandState(directory string, state commandState) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(directory, commandStateFile), append(encoded, '\n'), 0o600)
}

func commandStateMode(state commandState) xirangv1.NodeMode {
	mode, ok := nodeModeFromText(state.Mode)
	if !ok {
		return xirangv1.NodeMode_NODE_MODE_READY
	}
	return mode
}

func nodeModeText(mode xirangv1.NodeMode) (string, bool) {
	switch mode {
	case xirangv1.NodeMode_NODE_MODE_READY:
		return "ready", true
	case xirangv1.NodeMode_NODE_MODE_CORDONED:
		return "cordoned", true
	case xirangv1.NodeMode_NODE_MODE_DRAINING:
		return "draining", true
	default:
		return "", false
	}
}

func nodeModeFromText(mode string) (xirangv1.NodeMode, bool) {
	switch mode {
	case "ready":
		return xirangv1.NodeMode_NODE_MODE_READY, true
	case "cordoned":
		return xirangv1.NodeMode_NODE_MODE_CORDONED, true
	case "draining":
		return xirangv1.NodeMode_NODE_MODE_DRAINING, true
	default:
		return xirangv1.NodeMode_NODE_MODE_UNSPECIFIED, false
	}
}
