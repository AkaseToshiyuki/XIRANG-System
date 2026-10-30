// Package controlstream implements the long-lived, Agent-initiated control channel.
package controlstream

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/flow"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"github.com/xirang-grid/xirang/internal/pki"
	"github.com/xirang-grid/xirang/internal/telemetry"
	"github.com/xirang-grid/xirang/internal/topology"
	"github.com/xirang-grid/xirang/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const heartbeatCapability = "heartbeat-v1"

const nodeModeCapability = "node-mode-v1"

const nativeExecutorCapability = "native-executor-v1"

const cgroupExecutorCapability = "cgroup-v2-executor-v1"

const assignmentLogCapability = "assignment-log-snapshot-v1"

const deviceInventoryCapability = "device-inventory-v1"

const deviceExecutorCapability = "device-resource-executor-v1"

const cpusetExecutorCapability = "cpuset-affinity-v1"

const certificateRotationCapability = "certificate-rotation-v1"

const inventorySnapshotCapability = "inventory-snapshot-v1"

const resourceObservationCapability = "resource-observation-v1"

const interfaceHealthCapability = "interface-health-counters-v1"

const topologyObservationCapability = "topology-observation-v1"

const flowObservationCapability = "ebpf-flow-observation-v1"

const telemetryIntervalCapability = "telemetry-interval-v1"

const networkPathProbeCapability = "network-path-probe-v1"

const gangAssignmentCapability = "gang-assignment-v1"

const mpiLaunchCapability = "mpi-launch-v1"

const agentPTYCapability = "agent-pty-v1"

const remoteNativeBuildCapability = "remote-native-build-v1"

const managementPanelCapability = "management-panel-relay-v1"

const (
	MinTelemetryInterval = 2 * time.Second
	MaxTelemetryInterval = 60 * time.Second
)

type NodeHeartbeater interface {
	Heartbeat(context.Context, string, string) (domain.Node, error)
	GetNode(context.Context, string) (domain.Node, error)
}

type NodeCapabilityReporter interface {
	ReportCapabilities(context.Context, string, []string) error
}

type NodeVersionReporter interface {
	ReportAgentVersion(context.Context, string, string) error
}

type DeviceCapacityReporter interface {
	ReportDeviceCapacity(context.Context, string, map[string]int64) (domain.Node, error)
}

type HardwareTopologyReporter interface {
	ReportHardwareTopology(context.Context, string, map[string]int64, []domain.NUMANode, map[string]map[string]int64) (domain.Node, error)
}

type HardwarePerformanceReporter interface {
	ReportHardwarePerformance(context.Context, string, string, []string, float64, float64) (domain.Node, error)
}

type RuntimeFactsReporter interface {
	ReportRuntimeFacts(context.Context, string, map[string]string) (domain.Node, error)
}

type NodeModeHeartbeater interface {
	HeartbeatWithMode(context.Context, string, string, domain.NodeStatus) (domain.Node, error)
}

type NodeModeConfirmer interface {
	ConfirmNodeMode(context.Context, string, string, bool, string) (domain.Node, error)
}

type ManagementPanelReporter interface {
	ReportManagementPanel(context.Context, string, bool, string) (domain.Node, error)
}

type ManagementPanelConfirmer interface {
	ConfirmManagementPanel(context.Context, string, string, bool, string) (domain.Node, error)
}

type CertificateRotationConfirmer interface {
	ConfirmCertificateRotation(context.Context, string, string, bool, string) (domain.Node, error)
}

type AssignmentReporter interface {
	ReportAssignment(context.Context, string, *xirangv1.AssignmentUpdate) error
}

type AssignmentLogReporter interface {
	ReportAssignmentLog(context.Context, string, *xirangv1.AssignmentLogSnapshot) error
}

type AssignmentUsageReporter interface {
	ReportAssignmentUsage(context.Context, string, *xirangv1.AssignmentUsageSnapshot) error
}

type AssignmentCommandConfirmer interface {
	ConfirmAssignmentCommand(context.Context, string, *xirangv1.RunAssignment, bool, string) error
}

type AssignmentCancelConfirmer interface {
	ConfirmAssignmentCancellation(context.Context, string, *xirangv1.CancelAssignment, bool, string) error
}

type AssignmentReleaseConfirmer interface {
	ConfirmAssignmentRelease(context.Context, string, *xirangv1.ReleaseAssignment, bool, string) error
}

type TelemetryReporter interface {
	ReportInventory(context.Context, string, time.Time, *xirangv1.InventorySnapshot) error
	ReportObservation(context.Context, string, string, time.Time, *xirangv1.ResourceObservation) error
}

type TopologyReporter interface {
	Report(context.Context, string, string, time.Time, *xirangv1.TopologyObservation) (domain.TopologySnapshot, bool, error)
}

type NetworkProbeTargetProvider interface {
	NetworkProbeTargets(context.Context, string) ([]*xirangv1.NetworkProbeTarget, error)
}

type FlowReporter interface {
	Report(context.Context, string, string, time.Time, *xirangv1.FlowObservation) error
}

type TerminalReporter interface {
	ReportTerminalEvent(context.Context, string, *xirangv1.TerminalEvent) error
}

type MountReporter interface {
	ReportMountResult(context.Context, string, *xirangv1.SharedMountResult) error
}

type NativeBuildReporter interface {
	ReportWorker(string, *xirangv1.NativeBuildWorker) error
	ReportUpdate(string, *xirangv1.NativeBuildUpdate) error
	RemoveWorker(string)
}

type HostEventCommitter interface {
	CommitEvents(context.Context, string, *xirangv1.HostEventBatch) error
}

type HostHealthReporter interface {
	Report(context.Context, string, *xirangv1.HostHealthReport) error
	ConfirmAction(context.Context, string, string, bool, string) error
}

type Server struct {
	hostHealth HostHealthReporter
	xirangv1.UnimplementedAgentControlServer
	nodes             NodeHeartbeater
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	hub               *Hub
	telemetry         TelemetryReporter
	topology          TopologyReporter
	probeTargets      NetworkProbeTargetProvider
	flows             FlowReporter
	terminals         TerminalReporter
	mounts            MountReporter
	builds            NativeBuildReporter
	assignments       AssignmentReporter
	intervalMu        sync.RWMutex
	telemetryInterval time.Duration
	intervalPath      string
}

func (server *Server) SetHostHealthReporter(reporter HostHealthReporter) {
	server.hostHealth = reporter
}

func (server *Server) SetFlowReporter(reporter FlowReporter) {
	server.flows = reporter
}

func (server *Server) SetTerminalReporter(reporter TerminalReporter) {
	server.terminals = reporter
}

func (server *Server) SetMountReporter(reporter MountReporter) { server.mounts = reporter }

func (server *Server) SetNativeBuildReporter(reporter NativeBuildReporter) {
	server.builds = reporter
}

func (server *Server) SetAssignmentReporter(reporter AssignmentReporter) {
	server.assignments = reporter
}

func (server *Server) SetTelemetryReporter(reporter TelemetryReporter) {
	server.telemetry = reporter
}

func (server *Server) SetTopologyReporter(reporter TopologyReporter) {
	server.topology = reporter
}

func (server *Server) SetNetworkProbeTargetProvider(provider NetworkProbeTargetProvider) {
	server.probeTargets = provider
}

func New(nodes NodeHeartbeater, heartbeatInterval time.Duration, hubs ...*Hub) *Server {
	if heartbeatInterval < time.Second {
		heartbeatInterval = 10 * time.Second
	}
	telemetryInterval := heartbeatInterval
	if telemetryInterval < MinTelemetryInterval {
		telemetryInterval = MinTelemetryInterval
	}
	hub := NewHub(0)
	if len(hubs) > 0 && hubs[0] != nil {
		hub = hubs[0]
	}
	// Inventory and topology sampling in existing Agents can synchronously
	// take several seconds (including separate LLDP and route-query timeouts).
	// Keep authorization expiry bounded without treating those normal pauses
	// as missed heartbeats at the supported 1s reporting interval.
	return &Server{nodes: nodes, heartbeatInterval: heartbeatInterval, heartbeatTimeout: max(30*time.Second, 3*heartbeatInterval), telemetryInterval: telemetryInterval, hub: hub}
}

func (server *Server) ConfigureTelemetryInterval(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("telemetry interval path must be absolute")
	}
	interval := server.telemetryInterval
	data, err := os.ReadFile(path)
	if err == nil {
		interval, err = time.ParseDuration(strings.TrimSpace(string(data)))
		if err != nil || interval < MinTelemetryInterval || interval > MaxTelemetryInterval {
			return errors.New("persisted telemetry interval must be between 2s and 60s")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	server.intervalMu.Lock()
	server.intervalPath = path
	server.telemetryInterval = interval
	server.intervalMu.Unlock()
	return nil
}

func (server *Server) TelemetryInterval() time.Duration {
	server.intervalMu.RLock()
	defer server.intervalMu.RUnlock()
	return server.telemetryInterval
}

func (server *Server) UpdateTelemetryInterval(_ context.Context, interval time.Duration) error {
	if interval < MinTelemetryInterval || interval > MaxTelemetryInterval {
		return errors.New("telemetry interval must be between 2s and 60s")
	}
	server.intervalMu.RLock()
	path := server.intervalPath
	server.intervalMu.RUnlock()
	if path != "" {
		if err := persistTelemetryInterval(path, interval); err != nil {
			return err
		}
	}
	server.intervalMu.Lock()
	server.telemetryInterval = interval
	server.intervalMu.Unlock()
	for _, nodeID := range server.hub.ActiveNodeIDs() {
		if err := server.hub.Enqueue(nodeID, &xirangv1.ControlMessage{
			MessageId: newMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.ControlMessage_SetTelemetryInterval{SetTelemetryInterval: &xirangv1.SetTelemetryInterval{
				Interval: durationpb.New(interval),
			}},
		}); err != nil {
			return fmt.Errorf("update telemetry interval for node %s: %w", nodeID, err)
		}
	}
	return nil
}

func persistTelemetryInterval(path string, interval time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".telemetry-interval-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.WriteString(interval.String() + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (server *Server) Connect(stream grpc.BidiStreamingServer[xirangv1.AgentMessage, xirangv1.ControlMessage]) error {
	nodeID, certificate, err := authenticatedNode(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "Agent hello is required")
	}
	if first.MessageId == "" || first.SentAt == nil {
		return status.Error(codes.InvalidArgument, "Agent messages require an ID and timestamp")
	}
	if err := first.SentAt.CheckValid(); err != nil {
		return status.Error(codes.InvalidArgument, "Agent message timestamp is invalid")
	}
	hello := first.GetHello()
	if hello == nil || hello.NodeId != nodeID {
		return status.Error(codes.Unauthenticated, "hello identity does not match node certificate")
	}
	if hello.ProtocolVersion != version.ProtocolVersion {
		return status.Errorf(codes.FailedPrecondition, "unsupported protocol version %q", hello.ProtocolVersion)
	}
	if server.nodes == nil {
		return status.Error(codes.Unavailable, "node state service is unavailable")
	}
	var node domain.Node
	if modeHeartbeater, ok := server.nodes.(NodeModeHeartbeater); ok {
		node, err = modeHeartbeater.HeartbeatWithMode(stream.Context(), nodeID, certificate.SerialNumber.Text(16), "")
	} else {
		node, err = server.nodes.Heartbeat(stream.Context(), nodeID, certificate.SerialNumber.Text(16))
	}
	if err != nil {
		return status.Error(codes.Unauthenticated, "node certificate is not active")
	}
	if node.Status == domain.NodeStatusPendingApproval || node.Status == domain.NodeStatusRejected {
		return status.Error(codes.FailedPrecondition, "node is not approved for a control stream")
	}
	if reporter, ok := server.nodes.(NodeVersionReporter); ok {
		if err := reporter.ReportAgentVersion(stream.Context(), nodeID, hello.AgentVersion); err != nil {
			return status.Error(codes.Unavailable, "persist Agent version")
		}
	}
	negotiated := negotiateCapabilities(hello.Capabilities)
	if server.mounts == nil {
		filtered := negotiated[:0]
		for _, capability := range negotiated {
			if capability != mountcontrol.Capability {
				filtered = append(filtered, capability)
			}
		}
		negotiated = filtered
	}
	if _, ok := server.hostHealth.(HostEventCommitter); !ok {
		filtered := negotiated[:0]
		for _, capability := range negotiated {
			if capability != hosthealth.OutboxCapability {
				filtered = append(filtered, capability)
			}
		}
		negotiated = filtered
	}
	if server.hostHealth == nil {
		filtered := negotiated[:0]
		for _, capability := range negotiated {
			if capability != hosthealth.Capability && capability != hosthealth.IdentifyCapability && capability != hosthealth.OutboxCapability {
				filtered = append(filtered, capability)
			}
		}
		negotiated = filtered
	}
	if !hasCapability(negotiated, heartbeatCapability) {
		return status.Error(codes.FailedPrecondition, "heartbeat-v1 capability is required")
	}
	if reporter, ok := server.nodes.(NodeCapabilityReporter); ok {
		if err := reporter.ReportCapabilities(stream.Context(), nodeID, negotiated); err != nil {
			return status.Error(codes.Unavailable, "persist negotiated Agent capabilities")
		}
	}
	var probeTargets []*xirangv1.NetworkProbeTarget
	if hasCapability(negotiated, networkPathProbeCapability) && server.probeTargets != nil {
		probeTargets, err = server.probeTargets.NetworkProbeTargets(stream.Context(), nodeID)
		if err != nil {
			return status.Error(codes.Unavailable, "load network probe targets")
		}
	}
	if err := stream.Send(&xirangv1.ControlMessage{
		MessageId: newMessageID(), SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_HelloAccepted{HelloAccepted: &xirangv1.HelloAccepted{
			NodeId: nodeID, HeartbeatInterval: durationpb.New(server.heartbeatInterval),
			NegotiatedCapabilities: negotiated, TelemetryInterval: durationpb.New(server.TelemetryInterval()),
			NetworkProbeTargets: probeTargets,
		}},
	}); err != nil {
		return err
	}
	var lastHeartbeat atomic.Int64
	lastHeartbeat.Store(time.Now().UnixNano())
	authorized := func() bool {
		now := time.Now()
		if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) || now.Sub(time.Unix(0, lastHeartbeat.Load())) > server.heartbeatTimeout {
			return false
		}
		current, err := server.nodes.GetNode(stream.Context(), nodeID)
		return err == nil && current.CertificateRevokedAt == nil && current.CertificateSerial == certificate.SerialNumber.Text(16) && current.RemovedAt == nil &&
			current.Status != domain.NodeStatusRejected && current.Status != domain.NodeStatusPendingApproval && current.Status != domain.NodeStatusDiscovered
	}
	if !authorized() {
		return status.Error(codes.Unauthenticated, "node certificate is no longer active")
	}
	session := server.hub.attachAuthorized(nodeID, authorized)
	defer func() {
		if session.Detach() && server.builds != nil {
			server.builds.RemoveWorker(nodeID)
		}
	}()

	type receivedMessage struct {
		message *xirangv1.AgentMessage
		err     error
	}
	received := make(chan receivedMessage, 1)
	go func() {
		for {
			message, err := stream.Recv()
			select {
			case received <- receivedMessage{message: message, err: err}:
			case <-stream.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var lastSequence uint64
	validityTicker := time.NewTicker(min(server.heartbeatInterval, time.Second))
	defer validityTicker.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-session.Replaced():
			return status.Error(codes.Aborted, "control stream replaced by a newer connection")
		case <-validityTicker.C:
			if !authorized() {
				return status.Error(codes.Unauthenticated, "node authorization or heartbeat expired")
			}
		case <-session.Wake():
			if !authorized() {
				return status.Error(codes.Unauthenticated, "node authorization or heartbeat expired")
			}
			command, ok := session.Take()
			if !ok {
				continue
			}
			if command.GetRotateCertificate() != nil && !hasCapability(negotiated, certificateRotationCapability) {
				session.Requeue(command.MessageId)
				return status.Error(codes.FailedPrecondition, "Agent did not negotiate certificate-rotation-v1")
			}
			if (command.GetRunAssignment() != nil || command.GetCancelAssignment() != nil || command.GetReleaseAssignment() != nil || command.GetSetAssignmentStoragePolicy() != nil) && !hasCapability(negotiated, nativeExecutorCapability) {
				session.Requeue(command.MessageId)
				return status.Error(codes.FailedPrecondition, "Agent did not negotiate native-executor-v1")
			}
			if command.GetSetTelemetryInterval() != nil && !hasCapability(negotiated, telemetryIntervalCapability) {
				_, _ = session.Acknowledge(command.MessageId)
				continue
			}
			if command.GetUpdateNetworkProbeTargets() != nil && !hasCapability(negotiated, networkPathProbeCapability) {
				_, _ = session.Acknowledge(command.MessageId)
				continue
			}
			if (command.GetOpenTerminal() != nil || command.GetTerminalInput() != nil || command.GetResizeTerminal() != nil || command.GetCloseTerminal() != nil) && !hasCapability(negotiated, agentPTYCapability) {
				_, _ = session.Acknowledge(command.MessageId)
				continue
			}
			if command.GetRunNativeBuild() != nil && !hasCapability(negotiated, remoteNativeBuildCapability) {
				_, _ = session.Acknowledge(command.MessageId)
				continue
			}
			if command.GetSetBmcIdentify() != nil && !hasCapability(negotiated, hosthealth.IdentifyCapability) {
				session.Acknowledge(command.MessageId)
				continue
			}
			if operation := command.GetSharedMountOperation(); operation != nil {
				_, invalid := mountcontrol.Arguments(operation, time.Now())
				if invalid != nil || operation.OperationId != command.MessageId || !hasCapability(negotiated, mountcontrol.Capability) {
					session.Acknowledge(command.MessageId)
					if server.mounts != nil {
						_ = server.mounts.ReportMountResult(stream.Context(), nodeID, &xirangv1.SharedMountResult{OperationId: command.MessageId, Message: "mount command expired, invalid, or capability unavailable"})
					}
					continue
				}
			}
			if command.GetSetManagementPanel() != nil && !hasCapability(negotiated, managementPanelCapability) {
				_, _ = session.Acknowledge(command.MessageId)
				if confirmer, ok := server.nodes.(ManagementPanelConfirmer); ok {
					_, _ = confirmer.ConfirmManagementPanel(stream.Context(), nodeID, command.MessageId, false, "Agent does not support management-panel-relay-v1")
				}
				continue
			}
			if run := command.GetRunAssignment(); run != nil && run.Resources != nil && !hasCapability(negotiated, cgroupExecutorCapability) {
				session.Requeue(command.MessageId)
				return status.Error(codes.FailedPrecondition, "Agent did not negotiate cgroup-v2-executor-v1")
			}
			if run := command.GetRunAssignment(); run != nil && run.StartBarrier && !hasCapability(negotiated, gangAssignmentCapability) {
				session.Requeue(command.MessageId)
				return status.Error(codes.FailedPrecondition, "Agent did not negotiate gang-assignment-v1")
			}
			if err := stream.Send(command); err != nil {
				session.Requeue(command.MessageId)
				return err
			}
		case incoming := <-received:
			if !authorized() {
				return status.Error(codes.Unauthenticated, "node authorization or heartbeat expired")
			}
			if errors.Is(incoming.err, io.EOF) {
				return nil
			}
			if incoming.err != nil {
				return incoming.err
			}
			message := incoming.message
			if message.MessageId == "" || message.SentAt == nil {
				return status.Error(codes.InvalidArgument, "Agent messages require an ID and timestamp")
			}
			if err := message.SentAt.CheckValid(); err != nil {
				return status.Error(codes.InvalidArgument, "Agent message timestamp is invalid")
			}
			if heartbeat := message.GetHeartbeat(); heartbeat != nil {
				if heartbeat.Sequence == 0 || heartbeat.Sequence <= lastSequence {
					return status.Error(codes.InvalidArgument, "heartbeat sequence must increase")
				}
				reportedMode, ok := domainMode(heartbeat.Mode)
				if !ok {
					return status.Error(codes.InvalidArgument, "heartbeat contains an invalid node mode")
				}
				var err error
				if modeHeartbeater, ok := server.nodes.(NodeModeHeartbeater); ok {
					_, err = modeHeartbeater.HeartbeatWithMode(stream.Context(), nodeID, certificate.SerialNumber.Text(16), reportedMode)
				} else {
					_, err = server.nodes.Heartbeat(stream.Context(), nodeID, certificate.SerialNumber.Text(16))
				}
				if err != nil {
					return status.Error(codes.Unauthenticated, "node certificate is no longer active")
				}
				if reporter, ok := server.nodes.(ManagementPanelReporter); ok && hasCapability(negotiated, managementPanelCapability) {
					if _, err := reporter.ReportManagementPanel(stream.Context(), nodeID, heartbeat.ManagementPanelEnabled, heartbeat.ManagementPanelStatus); err != nil {
						return status.Error(codes.Unavailable, "persist management panel state")
					}
				}
				lastSequence = heartbeat.Sequence
				lastHeartbeat.Store(time.Now().UnixNano())
				continue
			}
			if inventory := message.GetInventory(); inventory != nil {
				if !hasCapability(negotiated, inventorySnapshotCapability) {
					return status.Error(codes.InvalidArgument, "inventory snapshots were not negotiated")
				}
				if server.telemetry == nil {
					return status.Error(codes.Unavailable, "inventory persistence is unavailable")
				}
				if !inventoryMatchesEnrolledCapacity(hello.Architecture, node.Capacity, inventory) {
					return status.Error(codes.InvalidArgument, "inventory snapshot does not match enrolled capacity")
				}
				if err := server.telemetry.ReportInventory(stream.Context(), nodeID, message.SentAt.AsTime(), inventory); err != nil {
					if errors.Is(err, telemetry.ErrInvalidReport) {
						return status.Error(codes.InvalidArgument, "inventory snapshot is invalid")
					}
					return status.Error(codes.Unavailable, "persist inventory snapshot")
				}
				if hasCapability(negotiated, deviceInventoryCapability) {
					if reporter, ok := server.nodes.(HardwareTopologyReporter); ok {
						if _, err := reporter.ReportHardwareTopology(stream.Context(), nodeID, inventoryDeviceQuantities(inventory), inventoryNUMATopology(inventory), inventoryDeviceTopology(inventory)); err != nil {
							return status.Error(codes.Unavailable, "persist device capacity")
						}
					} else if reporter, ok := server.nodes.(DeviceCapacityReporter); ok {
						if _, err := reporter.ReportDeviceCapacity(stream.Context(), nodeID, inventoryDeviceQuantities(inventory)); err != nil {
							return status.Error(codes.Unavailable, "persist device capacity")
						}
					}
				}
				if reporter, ok := server.nodes.(HardwarePerformanceReporter); ok {
					if _, err := reporter.ReportHardwarePerformance(stream.Context(), nodeID, inventory.CpuModel, inventory.CpuFeatures, inventory.CpuBenchmarkScore, inventory.ComputeWeight); err != nil {
						return status.Error(codes.Unavailable, "persist hardware performance")
					}
				}
				if reporter, ok := server.nodes.(RuntimeFactsReporter); ok {
					if _, err := reporter.ReportRuntimeFacts(stream.Context(), nodeID, inventory.RuntimeFacts); err != nil {
						return status.Error(codes.Unavailable, "persist runtime facts")
					}
				}
				server.refreshNetworkProbeTargets(stream.Context())
				continue
			}
			if observation := message.GetObservation(); observation != nil {
				if !hasCapability(negotiated, resourceObservationCapability) {
					return status.Error(codes.InvalidArgument, "resource observations were not negotiated")
				}
				if server.telemetry == nil {
					return status.Error(codes.Unavailable, "resource observation persistence is unavailable")
				}
				if observation.UsedMilliCpu < 0 || observation.UsedMilliCpu > node.Capacity.MilliCPU ||
					observation.UsedMemoryBytes > uint64(node.Capacity.MemoryBytes)+inventoryMemoryTolerance(node.Capacity.MemoryBytes) {
					return status.Error(codes.InvalidArgument, "resource observation exceeds node capacity")
				}
				if err := server.telemetry.ReportObservation(stream.Context(), nodeID, message.MessageId, message.SentAt.AsTime(), observation); err != nil {
					if errors.Is(err, telemetry.ErrInvalidReport) {
						return status.Error(codes.InvalidArgument, "resource observation is invalid")
					}
					return status.Error(codes.Unavailable, "persist resource observation")
				}
				continue
			}
			if batch := message.GetHostEvents(); batch != nil {
				committer, ok := server.hostHealth.(HostEventCommitter)
				if !ok || !hasCapability(negotiated, hosthealth.OutboxCapability) {
					return status.Error(codes.InvalidArgument, "host event outbox was not negotiated")
				}
				if err := committer.CommitEvents(stream.Context(), nodeID, batch); err != nil {
					if errors.Is(err, hosthealth.ErrInvalid) {
						return status.Error(codes.InvalidArgument, "invalid host event batch")
					}
					return status.Error(codes.Unavailable, "persist host event batch")
				}
				if err := stream.Send(&xirangv1.ControlMessage{MessageId: "events_" + batch.BatchId, SentAt: timestamppb.Now(), Payload: &xirangv1.ControlMessage_HostEventsCommitted{HostEventsCommitted: &xirangv1.HostEventsCommitted{BatchId: batch.BatchId}}}); err != nil {
					return err
				}
				continue
			}
			if report := message.GetHostHealth(); report != nil {
				if !hasCapability(negotiated, hosthealth.Capability) || server.hostHealth == nil {
					return status.Error(codes.InvalidArgument, "host health was not negotiated")
				}
				if err := server.hostHealth.Report(stream.Context(), nodeID, report); err != nil {
					if errors.Is(err, hosthealth.ErrInvalid) {
						return status.Error(codes.InvalidArgument, "invalid host health report")
					}
					return status.Error(codes.Unavailable, "persist host health report")
				}
				continue
			}
			if observation := message.GetTopology(); observation != nil {
				if !hasCapability(negotiated, topologyObservationCapability) {
					return status.Error(codes.InvalidArgument, "topology observations were not negotiated")
				}
				if server.topology == nil {
					return status.Error(codes.Unavailable, "topology persistence is unavailable")
				}
				if _, _, err := server.topology.Report(stream.Context(), nodeID, message.MessageId, message.SentAt.AsTime(), observation); err != nil {
					if errors.Is(err, topology.ErrInvalidReport) {
						return status.Error(codes.InvalidArgument, "topology observation is invalid")
					}
					return status.Error(codes.Unavailable, "persist topology observation")
				}
				continue
			}
			if observation := message.GetFlows(); observation != nil {
				if !hasCapability(negotiated, flowObservationCapability) {
					return status.Error(codes.InvalidArgument, "eBPF flow observations were not negotiated")
				}
				if server.flows == nil {
					return status.Error(codes.Unavailable, "flow observation persistence is unavailable")
				}
				if err := server.flows.Report(stream.Context(), nodeID, message.MessageId, message.SentAt.AsTime(), observation); err != nil {
					if errors.Is(err, flow.ErrInvalidReport) {
						return status.Error(codes.InvalidArgument, "flow observation is invalid")
					}
					return status.Error(codes.Unavailable, "persist flow observation")
				}
				continue
			}
			if result := message.GetSharedMountResult(); result != nil {
				if !hasCapability(negotiated, mountcontrol.Capability) || server.mounts == nil {
					return status.Error(codes.InvalidArgument, "shared mount results were not negotiated")
				}
				if err := server.mounts.ReportMountResult(stream.Context(), nodeID, result); err != nil {
					return status.Error(codes.InvalidArgument, "invalid shared mount result")
				}
				continue
			}
			if event := message.GetTerminalEvent(); event != nil {
				if !hasCapability(negotiated, agentPTYCapability) {
					return status.Error(codes.InvalidArgument, "Agent PTY events were not negotiated")
				}
				if server.terminals == nil {
					return status.Error(codes.Unavailable, "terminal event broker is unavailable")
				}
				if event.SessionId == "" || event.Type == xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_UNSPECIFIED || event.OccurredAt == nil || len(event.Data) > 64<<10 || len(event.Message) > 4096 {
					return status.Error(codes.InvalidArgument, "terminal event is invalid")
				}
				if err := server.terminals.ReportTerminalEvent(stream.Context(), nodeID, event); err != nil {
					return status.Error(codes.ResourceExhausted, "forward terminal event")
				}
				continue
			}
			if worker := message.GetNativeBuildWorker(); worker != nil {
				if !hasCapability(negotiated, remoteNativeBuildCapability) || server.builds == nil {
					return status.Error(codes.InvalidArgument, "remote native build worker was not negotiated")
				}
				if err := server.builds.ReportWorker(nodeID, worker); err != nil {
					return status.Error(codes.InvalidArgument, "native build worker report is invalid")
				}
				continue
			}
			if update := message.GetNativeBuildUpdate(); update != nil {
				if !hasCapability(negotiated, remoteNativeBuildCapability) || server.builds == nil {
					return status.Error(codes.InvalidArgument, "remote native build update was not negotiated")
				}
				if err := server.builds.ReportUpdate(nodeID, update); err != nil {
					return status.Error(codes.InvalidArgument, "native build update is invalid")
				}
				continue
			}
			if acknowledgement := message.GetAcknowledgement(); acknowledgement != nil {
				if acknowledgement.CommandMessageId == "" {
					return status.Error(codes.InvalidArgument, "command acknowledgement requires a message ID")
				}
				command, ok := session.Acknowledge(acknowledgement.CommandMessageId)
				if !ok {
					continue
				}
				if command.GetSharedMountOperation() != nil {
					if !acknowledgement.Accepted && server.mounts != nil {
						reason := acknowledgement.Reason
						if len(reason) > 1024 {
							reason = reason[:1024]
						}
						if err := server.mounts.ReportMountResult(stream.Context(), nodeID, &xirangv1.SharedMountResult{OperationId: command.MessageId, Message: reason}); err != nil {
							return status.Error(codes.InvalidArgument, "invalid shared mount rejection")
						}
					}
				} else if command.GetSetBmcIdentify() != nil {
					if server.hostHealth == nil {
						return status.Error(codes.Unavailable, "BMC action persistence unavailable")
					}
					if err := server.hostHealth.ConfirmAction(stream.Context(), nodeID, acknowledgement.CommandMessageId, acknowledgement.Accepted, acknowledgement.Reason); err != nil {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "persist BMC acknowledgement")
					}
				} else if command.GetSetNodeMode() != nil {
					confirmer, ok := server.nodes.(NodeModeConfirmer)
					if !ok {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "node mode persistence is unavailable")
					}
					if _, err := confirmer.ConfirmNodeMode(stream.Context(), nodeID, acknowledgement.CommandMessageId, acknowledgement.Accepted, acknowledgement.Reason); err != nil {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "persist node mode acknowledgement")
					}
				} else if command.GetRotateCertificate() != nil {
					confirmer, ok := server.nodes.(CertificateRotationConfirmer)
					if !ok {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "certificate rotation persistence is unavailable")
					}
					if _, err := confirmer.ConfirmCertificateRotation(stream.Context(), nodeID, acknowledgement.CommandMessageId, acknowledgement.Accepted, acknowledgement.Reason); err != nil {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "persist certificate rotation acknowledgement")
					}
				} else if command.GetSetManagementPanel() != nil {
					confirmer, ok := server.nodes.(ManagementPanelConfirmer)
					if !ok {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "management panel persistence is unavailable")
					}
					if _, err := confirmer.ConfirmManagementPanel(stream.Context(), nodeID, acknowledgement.CommandMessageId, acknowledgement.Accepted, acknowledgement.Reason); err != nil {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "persist management panel acknowledgement")
					}
				} else if assignment := command.GetRunAssignment(); assignment != nil {
					confirmer, ok := server.assignments.(AssignmentCommandConfirmer)
					if !ok {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "assignment persistence is unavailable")
					}
					if err := confirmer.ConfirmAssignmentCommand(stream.Context(), nodeID, assignment, acknowledgement.Accepted, acknowledgement.Reason); err != nil {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "persist assignment acknowledgement")
					}
				} else if cancellation := command.GetCancelAssignment(); cancellation != nil {
					confirmer, ok := server.assignments.(AssignmentCancelConfirmer)
					if !ok {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "assignment cancellation persistence is unavailable")
					}
					if err := confirmer.ConfirmAssignmentCancellation(stream.Context(), nodeID, cancellation, acknowledgement.Accepted, acknowledgement.Reason); err != nil {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "persist assignment cancellation acknowledgement")
					}
				} else if release := command.GetReleaseAssignment(); release != nil {
					confirmer, ok := server.assignments.(AssignmentReleaseConfirmer)
					if !ok {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "assignment release persistence is unavailable")
					}
					if err := confirmer.ConfirmAssignmentRelease(stream.Context(), nodeID, release, acknowledgement.Accepted, acknowledgement.Reason); err != nil {
						_ = server.hub.Enqueue(nodeID, command)
						return status.Error(codes.Unavailable, "persist assignment release acknowledgement")
					}
				}
				continue
			}
			if update := message.GetAssignmentUpdate(); update != nil {
				if !hasCapability(negotiated, nativeExecutorCapability) {
					return status.Error(codes.InvalidArgument, "assignment updates were not negotiated")
				}
				if update.AssignmentId == "" || update.Attempt == 0 || update.State == xirangv1.AssignmentState_ASSIGNMENT_STATE_UNSPECIFIED || update.OccurredAt == nil {
					return status.Error(codes.InvalidArgument, "assignment update is incomplete")
				}
				reporter := server.assignments
				if reporter == nil {
					reporter, _ = server.nodes.(AssignmentReporter)
				}
				if reporter != nil {
					if err := reporter.ReportAssignment(stream.Context(), nodeID, update); err != nil {
						return status.Error(codes.Unavailable, "persist assignment update")
					}
				}
				continue
			}
			if snapshot := message.GetAssignmentLog(); snapshot != nil {
				if !hasCapability(negotiated, assignmentLogCapability) {
					return status.Error(codes.InvalidArgument, "assignment log snapshots were not negotiated")
				}
				if snapshot.AssignmentId == "" || snapshot.Attempt == 0 || snapshot.ObservedAt == nil {
					return status.Error(codes.InvalidArgument, "assignment log snapshot is incomplete")
				}
				reporter, _ := server.assignments.(AssignmentLogReporter)
				if reporter == nil {
					reporter, _ = server.nodes.(AssignmentLogReporter)
				}
				if reporter != nil {
					if err := reporter.ReportAssignmentLog(stream.Context(), nodeID, snapshot); err != nil {
						return status.Error(codes.Unavailable, "persist assignment log snapshot")
					}
				}
				continue
			}
			if snapshot := message.GetAssignmentUsage(); snapshot != nil {
				if !hasCapability(negotiated, nativeExecutorCapability) {
					return status.Error(codes.InvalidArgument, "assignment usage snapshots were not negotiated")
				}
				if snapshot.AssignmentId == "" || snapshot.Attempt == 0 || snapshot.ObservedAt == nil {
					return status.Error(codes.InvalidArgument, "assignment usage snapshot is incomplete")
				}
				reporter, _ := server.assignments.(AssignmentUsageReporter)
				if reporter == nil {
					reporter, _ = server.nodes.(AssignmentUsageReporter)
				}
				if reporter != nil {
					if err := reporter.ReportAssignmentUsage(stream.Context(), nodeID, snapshot); err != nil {
						return status.Error(codes.Unavailable, "persist assignment usage snapshot")
					}
				}
				continue
			}
			return status.Error(codes.InvalidArgument, "message type was not negotiated")
		}
	}
}

// Linux MemTotal can move slightly after a reboot as firmware reservations,
// kernel metadata, and device drivers change. Enrollment remains authoritative,
// but a small bounded drift must not tear down the entire telemetry stream.
func inventoryMatchesEnrolledCapacity(architecture string, capacity domain.ResourceVector, inventory *xirangv1.InventorySnapshot) bool {
	if inventory == nil || architecture == "" || inventory.Architecture != architecture ||
		inventory.LogicalCpuCount == 0 || int64(inventory.LogicalCpuCount)*1000 != capacity.MilliCPU ||
		inventory.MemoryBytes == 0 || capacity.MemoryBytes <= 0 {
		return false
	}
	enrolled := uint64(capacity.MemoryBytes)
	var difference uint64
	if inventory.MemoryBytes > enrolled {
		difference = inventory.MemoryBytes - enrolled
	} else {
		difference = enrolled - inventory.MemoryBytes
	}
	return difference <= inventoryMemoryTolerance(capacity.MemoryBytes)
}

func inventoryMemoryTolerance(memoryBytes int64) uint64 {
	const minimum = uint64(64 << 20)
	if memoryBytes <= 0 {
		return minimum
	}
	relative := uint64(memoryBytes) / 200 // 0.5 percent.
	if relative > minimum {
		return relative
	}
	return minimum
}

func (server *Server) refreshNetworkProbeTargets(ctx context.Context) {
	if server.probeTargets == nil {
		return
	}
	for _, nodeID := range server.hub.ActiveNodeIDs() {
		targets, err := server.probeTargets.NetworkProbeTargets(ctx, nodeID)
		if err != nil {
			continue
		}
		_ = server.hub.Enqueue(nodeID, &xirangv1.ControlMessage{
			MessageId: newMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.ControlMessage_UpdateNetworkProbeTargets{
				UpdateNetworkProbeTargets: &xirangv1.UpdateNetworkProbeTargets{Targets: targets},
			},
		})
	}
}

func domainMode(mode xirangv1.NodeMode) (domain.NodeStatus, bool) {
	switch mode {
	case xirangv1.NodeMode_NODE_MODE_READY:
		return domain.NodeStatusReady, true
	case xirangv1.NodeMode_NODE_MODE_CORDONED:
		return domain.NodeStatusCordoned, true
	case xirangv1.NodeMode_NODE_MODE_DRAINING:
		return domain.NodeStatusDraining, true
	default:
		return "", false
	}
}

func hasCapability(capabilities []string, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func authenticatedNode(ctx context.Context) (string, *x509.Certificate, error) {
	remote, ok := peer.FromContext(ctx)
	if !ok {
		return "", nil, status.Error(codes.Unauthenticated, "verified client certificate is required")
	}
	tlsInfo, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 || len(tlsInfo.State.VerifiedChains) == 0 {
		return "", nil, status.Error(codes.Unauthenticated, "verified client certificate is required")
	}
	certificate := tlsInfo.State.PeerCertificates[0]
	nodeID, ok := pki.NodeIDFromCertificate(certificate)
	if !ok {
		return "", nil, status.Error(codes.Unauthenticated, "node URI identity is required")
	}
	return nodeID, certificate, nil
}

func negotiateCapabilities(offered []string) []string {
	negotiated := make([]string, 0, 9)
	for _, capability := range offered {
		switch capability {
		case mountcontrol.Capability, hosthealth.Capability, hosthealth.IdentifyCapability, hosthealth.OutboxCapability, heartbeatCapability, nodeModeCapability, nativeExecutorCapability, cgroupExecutorCapability, certificateRotationCapability,
			inventorySnapshotCapability, resourceObservationCapability, interfaceHealthCapability, topologyObservationCapability,
			flowObservationCapability, assignmentLogCapability, deviceInventoryCapability, telemetryIntervalCapability,
			networkPathProbeCapability, gangAssignmentCapability, mpiLaunchCapability, agentPTYCapability, remoteNativeBuildCapability,
			managementPanelCapability:
			negotiated = append(negotiated, capability)
		case deviceExecutorCapability, cpusetExecutorCapability:
			negotiated = append(negotiated, capability)
		default:
			if strings.HasPrefix(capability, deviceExecutorCapability+"/") && len(capability) <= 256 {
				negotiated = append(negotiated, capability)
			}
		}
	}
	return negotiated
}

func inventoryDeviceQuantities(inventory *xirangv1.InventorySnapshot) map[string]int64 {
	quantities := make(map[string]int64)
	for _, device := range inventory.GetDevices() {
		if device.GetResourceName() != "" {
			quantities[device.GetResourceName()]++
		}
	}
	if len(quantities) == 0 {
		return nil
	}
	return quantities
}

func inventoryNUMATopology(inventory *xirangv1.InventorySnapshot) []domain.NUMANode {
	nodes := make([]domain.NUMANode, 0, len(inventory.GetNumaNodes()))
	for _, node := range inventory.GetNumaNodes() {
		if node != nil {
			nodes = append(nodes, domain.NUMANode{ID: node.Id, CPUs: node.Cpus, MemoryBytes: int64(node.MemoryBytes)})
		}
	}
	return nodes
}

func inventoryDeviceTopology(inventory *xirangv1.InventorySnapshot) map[string]map[string]int64 {
	topology := make(map[string]map[string]int64)
	for _, device := range inventory.GetDevices() {
		numa := device.GetAttributes()["numaNode"]
		if device.GetResourceName() == "" || numa == "" {
			continue
		}
		if topology[device.GetResourceName()] == nil {
			topology[device.GetResourceName()] = make(map[string]int64)
		}
		topology[device.GetResourceName()][numa]++
	}
	if len(topology) == 0 {
		return nil
	}
	return topology
}

func newMessageID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "control_" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "control_" + hex.EncodeToString(buffer)
}
