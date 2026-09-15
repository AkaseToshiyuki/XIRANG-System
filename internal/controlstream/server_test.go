package controlstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTelemetryIntervalPersistsAcrossControllerRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-interval")
	first := New(&testHeartbeater{}, 10*time.Second)
	if err := first.ConfigureTelemetryInterval(path); err != nil {
		t.Fatal(err)
	}
	if err := first.UpdateTelemetryInterval(context.Background(), 7*time.Second); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "7s\n" {
		t.Fatalf("unexpected persisted interval data=%q err=%v", data, err)
	}
	second := New(&testHeartbeater{}, 10*time.Second)
	if err := second.ConfigureTelemetryInterval(path); err != nil {
		t.Fatal(err)
	}
	if interval := second.TelemetryInterval(); interval != 7*time.Second {
		t.Fatalf("restored interval = %s", interval)
	}
}

func TestTelemetryIntervalDispatchesToConnectedAgents(t *testing.T) {
	hub := NewHub(8)
	session := hub.Attach("node_test")
	defer session.Detach()
	server := New(&testHeartbeater{}, 10*time.Second, hub)
	if err := server.UpdateTelemetryInterval(context.Background(), 4*time.Second); err != nil {
		t.Fatal(err)
	}
	command, ok := session.Take()
	if !ok || command.GetSetTelemetryInterval() == nil ||
		command.GetSetTelemetryInterval().GetInterval().AsDuration() != 4*time.Second {
		t.Fatalf("unexpected telemetry interval command: %#v", command)
	}
}

func TestConnectNegotiatesAndAcceptsIncreasingHeartbeat(t *testing.T) {
	nodes := &testHeartbeater{}
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{
		helloMessage("node_test"), heartbeatMessage(1), heartbeatMessage(2),
	})
	if err := New(nodes, 3*time.Second).Connect(stream); err != nil {
		t.Fatal(err)
	}
	if nodes.calls != 3 || nodes.serial != "2a" {
		t.Fatalf("unexpected heartbeat calls=%d serial=%q", nodes.calls, nodes.serial)
	}
	if nodes.lastMode != domain.NodeStatusReady {
		t.Fatalf("unexpected reported mode: %q", nodes.lastMode)
	}
	if nodes.agentVersion != "test-version" {
		t.Fatalf("Agent version was not persisted: %q", nodes.agentVersion)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("expected one hello acknowledgement, got %d", len(stream.sent))
	}
	accepted := stream.sent[0].GetHelloAccepted()
	if accepted == nil || accepted.NodeId != "node_test" || accepted.HeartbeatInterval.AsDuration() != 3*time.Second {
		t.Fatalf("unexpected hello acknowledgement: %#v", stream.sent[0])
	}
	if len(accepted.NegotiatedCapabilities) != 1 || accepted.NegotiatedCapabilities[0] != heartbeatCapability {
		t.Fatalf("unexpected capabilities: %#v", accepted.NegotiatedCapabilities)
	}
}

func TestConnectRejectsCertificateHelloMismatch(t *testing.T) {
	stream := newTestStream("node_certificate", []*xirangv1.AgentMessage{helloMessage("node_claimed")})
	err := New(&testHeartbeater{}, time.Second).Connect(stream)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
}

func TestConnectRejectsReplayedHeartbeatSequence(t *testing.T) {
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{
		helloMessage("node_test"), heartbeatMessage(1), heartbeatMessage(1),
	})
	err := New(&testHeartbeater{}, time.Second).Connect(stream)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected invalid argument, got %v", err)
	}
}

func TestConnectRequiresApprovedNode(t *testing.T) {
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{helloMessage("node_test")})
	err := New(&testHeartbeater{status: domain.NodeStatusPendingApproval}, time.Second).Connect(stream)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected failed precondition, got %v", err)
	}
}

func TestConnectDeliversQueuedCommandAndConsumesAcknowledgement(t *testing.T) {
	hub := NewHub(4)
	command := testModeCommand("command-delivery", xirangv1.NodeMode_NODE_MODE_CORDONED)
	if err := hub.Enqueue("node_test", command); err != nil {
		t.Fatal(err)
	}
	stream := newAsyncTestStream("node_test")
	done := make(chan error, 1)
	go func() { done <- New(&testHeartbeater{}, time.Second, hub).Connect(stream) }()
	stream.incoming <- helloMessage("node_test")
	if message := <-stream.outgoing; message.GetHelloAccepted() == nil {
		t.Fatalf("expected hello acknowledgement, got %#v", message)
	}
	delivered := <-stream.outgoing
	if delivered.MessageId != command.MessageId || delivered.GetSetNodeMode() == nil {
		t.Fatalf("unexpected delivered command: %#v", delivered)
	}
	stream.incoming <- &xirangv1.AgentMessage{
		MessageId: "ack-1", SentAt: timestamppb.Now(),
		Payload: &xirangv1.AgentMessage_Acknowledgement{Acknowledgement: &xirangv1.CommandAcknowledgement{
			CommandMessageId: command.MessageId, Accepted: true,
		}},
	}
	close(stream.incoming)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	reconnected := hub.Attach("node_test")
	defer reconnected.Detach()
	if replayed, ok := reconnected.Take(); ok {
		t.Fatalf("acknowledged command was replayed: %#v", replayed)
	}
}

func TestConnectNegotiatesAndReportsAssignmentUpdate(t *testing.T) {
	nodes := &testHeartbeater{}
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, nativeExecutorCapability)
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{
		hello,
		{
			MessageId: "assignment-update-1", SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_AssignmentUpdate{AssignmentUpdate: &xirangv1.AssignmentUpdate{
				AssignmentId: "assignment-1", Attempt: 1,
				State: xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING, OccurredAt: timestamppb.Now(),
			}},
		},
	})
	if err := New(nodes, time.Second).Connect(stream); err != nil {
		t.Fatal(err)
	}
	if nodes.assignment == nil || nodes.assignment.AssignmentId != "assignment-1" || nodes.assignmentNodeID != "node_test" {
		t.Fatalf("assignment update was not reported: node=%q update=%#v", nodes.assignmentNodeID, nodes.assignment)
	}
	accepted := stream.sent[0].GetHelloAccepted()
	if !hasCapability(accepted.NegotiatedCapabilities, nativeExecutorCapability) {
		t.Fatalf("native executor capability was not negotiated: %#v", accepted.NegotiatedCapabilities)
	}
}

func TestConnectNegotiatesAndReportsAssignmentLogSnapshot(t *testing.T) {
	nodes := &testHeartbeater{}
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, nativeExecutorCapability, assignmentLogCapability)
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{
		hello,
		{
			MessageId: "assignment-log-1", SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_AssignmentLog{AssignmentLog: &xirangv1.AssignmentLogSnapshot{
				AssignmentId: "assignment-1", Attempt: 1, StdoutTail: []byte("live\n"), ObservedAt: timestamppb.Now(),
			}},
		},
	})
	if err := New(nodes, time.Second).Connect(stream); err != nil {
		t.Fatal(err)
	}
	if nodes.assignmentLog == nil || string(nodes.assignmentLog.StdoutTail) != "live\n" || nodes.assignmentNodeID != "node_test" {
		t.Fatalf("assignment log was not reported: node=%q snapshot=%#v", nodes.assignmentNodeID, nodes.assignmentLog)
	}
}

func TestConnectPersistsRunAssignmentAcknowledgement(t *testing.T) {
	hub := NewHub(4)
	command := &xirangv1.ControlMessage{
		MessageId: "command-run-1", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RunAssignment{RunAssignment: &xirangv1.RunAssignment{
			AssignmentId: "assignment-1", JobId: "job-1", Attempt: 1,
		}},
	}
	if err := hub.Enqueue("node_test", command); err != nil {
		t.Fatal(err)
	}
	reporter := &testAssignmentReporter{}
	server := New(&testHeartbeater{}, time.Second, hub)
	server.SetAssignmentReporter(reporter)
	stream := newAsyncTestStream("node_test")
	done := make(chan error, 1)
	go func() { done <- server.Connect(stream) }()
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, nativeExecutorCapability)
	stream.incoming <- hello
	if message := <-stream.outgoing; message.GetHelloAccepted() == nil {
		t.Fatalf("expected hello acknowledgement, got %#v", message)
	}
	if delivered := <-stream.outgoing; delivered.MessageId != command.MessageId || delivered.GetRunAssignment() == nil {
		t.Fatalf("unexpected assignment command: %#v", delivered)
	}
	stream.incoming <- &xirangv1.AgentMessage{
		MessageId: "ack-run-1", SentAt: timestamppb.Now(),
		Payload: &xirangv1.AgentMessage_Acknowledgement{Acknowledgement: &xirangv1.CommandAcknowledgement{
			CommandMessageId: command.MessageId, Accepted: false, Reason: "artifact unavailable",
		}},
	}
	close(stream.incoming)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reporter.command == nil || reporter.command.AssignmentId != "assignment-1" || reporter.accepted || reporter.reason != "artifact unavailable" {
		t.Fatalf("assignment acknowledgement was not persisted: %#v", reporter)
	}
}

type testAssignmentReporter struct {
	command      *xirangv1.RunAssignment
	cancellation *xirangv1.CancelAssignment
	accepted     bool
	reason       string
}

func (reporter *testAssignmentReporter) ConfirmAssignmentCancellation(_ context.Context, _ string, command *xirangv1.CancelAssignment, accepted bool, reason string) error {
	reporter.cancellation, reporter.accepted, reporter.reason = command, accepted, reason
	return nil
}

func TestConnectPersistsAssignmentCancellationAcknowledgement(t *testing.T) {
	hub := NewHub(4)
	command := &xirangv1.ControlMessage{
		MessageId: "command-cancel-1", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_CancelAssignment{CancelAssignment: &xirangv1.CancelAssignment{AssignmentId: "assignment-1"}},
	}
	if err := hub.Enqueue("node_test", command); err != nil {
		t.Fatal(err)
	}
	reporter := &testAssignmentReporter{}
	server := New(&testHeartbeater{}, time.Second, hub)
	server.SetAssignmentReporter(reporter)
	stream := newAsyncTestStream("node_test")
	done := make(chan error, 1)
	go func() { done <- server.Connect(stream) }()
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, nativeExecutorCapability)
	stream.incoming <- hello
	<-stream.outgoing
	if delivered := <-stream.outgoing; delivered.GetCancelAssignment() == nil {
		t.Fatalf("unexpected cancellation command: %#v", delivered)
	}
	stream.incoming <- &xirangv1.AgentMessage{
		MessageId: "ack-cancel-1", SentAt: timestamppb.Now(),
		Payload: &xirangv1.AgentMessage_Acknowledgement{Acknowledgement: &xirangv1.CommandAcknowledgement{
			CommandMessageId: command.MessageId, Accepted: true,
		}},
	}
	close(stream.incoming)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if reporter.cancellation == nil || reporter.cancellation.AssignmentId != "assignment-1" || !reporter.accepted {
		t.Fatalf("cancellation acknowledgement was not persisted: %#v", reporter)
	}
}

func (reporter *testAssignmentReporter) ReportAssignment(context.Context, string, *xirangv1.AssignmentUpdate) error {
	return nil
}

func (reporter *testAssignmentReporter) ConfirmAssignmentCommand(_ context.Context, _ string, command *xirangv1.RunAssignment, accepted bool, reason string) error {
	reporter.command, reporter.accepted, reporter.reason = command, accepted, reason
	return nil
}

func TestConnectNegotiatesAndPersistsTelemetry(t *testing.T) {
	nodes := &testHeartbeater{capacity: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30}}
	reporter := &testTelemetryReporter{}
	hello := helloMessage("node_test")
	hello.GetHello().Architecture = "amd64"
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, inventorySnapshotCapability, resourceObservationCapability, interfaceHealthCapability, deviceInventoryCapability)
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{
		hello,
		{
			MessageId: "inventory-1", SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Inventory{Inventory: &xirangv1.InventorySnapshot{
				Hostname: "compute-a", Architecture: "amd64", LogicalCpuCount: 8, MemoryBytes: 16 << 30,
				Devices:   []*xirangv1.Device{{ResourceName: "nvidia.com/gpu"}, {ResourceName: "nvidia.com/gpu"}},
				NumaNodes: []*xirangv1.NUMANode{{Id: 0, Cpus: "0-7", MemoryBytes: 16 << 30}},
			}},
		},
		{
			MessageId: "observation-1", SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Observation{Observation: &xirangv1.ResourceObservation{
				UsedMilliCpu: 2_500, UsedMemoryBytes: 4 << 30, ReceivedBytes: 100, TransmittedBytes: 200,
			}},
		},
	})
	server := New(nodes, time.Second)
	server.SetTelemetryReporter(reporter)
	if err := server.Connect(stream); err != nil {
		t.Fatal(err)
	}
	if reporter.inventory == nil || reporter.inventoryNodeID != "node_test" {
		t.Fatalf("inventory was not persisted: node=%q inventory=%#v", reporter.inventoryNodeID, reporter.inventory)
	}
	if reporter.observation == nil || reporter.observationMessageID != "observation-1" {
		t.Fatalf("observation was not persisted: message=%q observation=%#v", reporter.observationMessageID, reporter.observation)
	}
	if nodes.deviceCapacity["nvidia.com/gpu"] != 2 {
		t.Fatalf("device capacity was not persisted: %#v", nodes.deviceCapacity)
	}
	if len(nodes.numaTopology) != 1 || nodes.numaTopology[0].CPUs != "0-7" {
		t.Fatalf("NUMA topology was not persisted: %#v", nodes.numaTopology)
	}
	accepted := stream.sent[0].GetHelloAccepted()
	if !hasCapability(accepted.NegotiatedCapabilities, inventorySnapshotCapability) ||
		!hasCapability(accepted.NegotiatedCapabilities, resourceObservationCapability) ||
		!hasCapability(accepted.NegotiatedCapabilities, interfaceHealthCapability) ||
		!hasCapability(accepted.NegotiatedCapabilities, deviceInventoryCapability) {
		t.Fatalf("telemetry capabilities were not negotiated: %#v", accepted.NegotiatedCapabilities)
	}
}

func TestInventoryCapacityAllowsBoundedLinuxMemoryDrift(t *testing.T) {
	capacity := domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 16 << 30}
	inventory := &xirangv1.InventorySnapshot{
		Architecture: "amd64", LogicalCpuCount: 8,
		MemoryBytes: uint64(capacity.MemoryBytes - (32 << 20)),
	}
	if !inventoryMatchesEnrolledCapacity("amd64", capacity, inventory) {
		t.Fatal("bounded MemTotal drift should not disconnect an enrolled Agent")
	}
	inventory.MemoryBytes = uint64(capacity.MemoryBytes - (512 << 20))
	if inventoryMatchesEnrolledCapacity("amd64", capacity, inventory) {
		t.Fatal("large memory capacity changes must require explicit re-enrollment")
	}
	inventory.MemoryBytes = uint64(capacity.MemoryBytes)
	inventory.LogicalCpuCount = 7
	if inventoryMatchesEnrolledCapacity("amd64", capacity, inventory) {
		t.Fatal("CPU capacity changes must require explicit re-enrollment")
	}
}

func TestConnectNegotiatesAndPersistsTopology(t *testing.T) {
	nodes := &testHeartbeater{}
	reporter := &testTopologyReporter{}
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, topologyObservationCapability)
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{
		hello,
		{
			MessageId: "topology-1", SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Topology{Topology: &xirangv1.TopologyObservation{
				Routes: []*xirangv1.Route{{Destination: "0.0.0.0/0", Gateway: "10.0.0.1", InterfaceName: "eth0", Table: 254, Family: "ipv4"}},
			}},
		},
	})
	server := New(nodes, time.Second)
	server.SetTopologyReporter(reporter)
	if err := server.Connect(stream); err != nil {
		t.Fatal(err)
	}
	if reporter.nodeID != "node_test" || reporter.messageID != "topology-1" || reporter.observation == nil {
		t.Fatalf("topology was not persisted: %#v", reporter)
	}
	if !hasCapability(stream.sent[0].GetHelloAccepted().NegotiatedCapabilities, topologyObservationCapability) {
		t.Fatal("topology capability was not negotiated")
	}
}

func TestConnectNegotiatesAndPersistsFlows(t *testing.T) {
	nodes := &testHeartbeater{}
	reporter := &testFlowReporter{}
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, flowObservationCapability)
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{
		hello,
		{
			MessageId: "flows-1", SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Flows{Flows: &xirangv1.FlowObservation{
				Interval: durationpb.New(10 * time.Second),
				Flows: []*xirangv1.NetworkFlow{{
					Family: "ipv4", Protocol: "tcp", Direction: "egress",
					SourceAddress: "10.0.0.1", DestinationAddress: "10.0.0.2", Packets: 2, Bytes: 128,
				}},
			}},
		},
	})
	server := New(nodes, time.Second)
	server.SetFlowReporter(reporter)
	if err := server.Connect(stream); err != nil {
		t.Fatal(err)
	}
	if reporter.nodeID != "node_test" || reporter.messageID != "flows-1" || reporter.observation == nil {
		t.Fatalf("flows were not persisted: %#v", reporter)
	}
	if !hasCapability(stream.sent[0].GetHelloAccepted().NegotiatedCapabilities, flowObservationCapability) {
		t.Fatal("flow capability was not negotiated")
	}
}

func TestConnectPersistsCertificateRotationAcknowledgement(t *testing.T) {
	hub := NewHub(4)
	command := &xirangv1.ControlMessage{
		MessageId: "command-rotate", SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_RotateCertificate{RotateCertificate: &xirangv1.RotateCertificate{RotationId: "rotation-1"}},
	}
	if err := hub.Enqueue("node_test", command); err != nil {
		t.Fatal(err)
	}
	nodes := &testHeartbeater{}
	stream := newAsyncTestStream("node_test")
	done := make(chan error, 1)
	go func() { done <- New(nodes, time.Second, hub).Connect(stream) }()
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, certificateRotationCapability)
	stream.incoming <- hello
	if message := <-stream.outgoing; message.GetHelloAccepted() == nil {
		t.Fatalf("expected hello acknowledgement, got %#v", message)
	}
	delivered := <-stream.outgoing
	if delivered.MessageId != command.MessageId || delivered.GetRotateCertificate() == nil {
		t.Fatalf("unexpected delivered command: %#v", delivered)
	}
	stream.incoming <- &xirangv1.AgentMessage{
		MessageId: "ack-rotate", SentAt: timestamppb.Now(),
		Payload: &xirangv1.AgentMessage_Acknowledgement{Acknowledgement: &xirangv1.CommandAcknowledgement{
			CommandMessageId: command.MessageId, Accepted: true,
		}},
	}
	close(stream.incoming)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if nodes.rotationCommandID != command.MessageId || !nodes.rotationAccepted {
		t.Fatalf("certificate rotation acknowledgement was not persisted: command=%q accepted=%t", nodes.rotationCommandID, nodes.rotationAccepted)
	}
}

type testHeartbeater struct {
	calls             int
	serial            string
	status            domain.NodeStatus
	lastMode          domain.NodeStatus
	assignmentNodeID  string
	assignment        *xirangv1.AssignmentUpdate
	assignmentLog     *xirangv1.AssignmentLogSnapshot
	rotationCommandID string
	rotationAccepted  bool
	capacity          domain.ResourceVector
	deviceCapacity    map[string]int64
	numaTopology      []domain.NUMANode
	agentVersion      string
}

func (service *testHeartbeater) ReportAgentVersion(_ context.Context, _ string, agentVersion string) error {
	service.agentVersion = agentVersion
	return nil
}

func (service *testHeartbeater) ReportDeviceCapacity(_ context.Context, id string, devices map[string]int64) (domain.Node, error) {
	service.deviceCapacity = devices
	return domain.Node{ID: id, Status: domain.NodeStatusReady}, nil
}

func (service *testHeartbeater) ReportHardwareTopology(_ context.Context, id string, devices map[string]int64, numa []domain.NUMANode, _ map[string]map[string]int64) (domain.Node, error) {
	service.deviceCapacity, service.numaTopology = devices, numa
	return domain.Node{ID: id, Status: domain.NodeStatusReady}, nil
}

func (service *testHeartbeater) ConfirmCertificateRotation(_ context.Context, _ string, commandID string, accepted bool, _ string) (domain.Node, error) {
	service.rotationCommandID = commandID
	service.rotationAccepted = accepted
	return domain.Node{ID: "node_test", Status: domain.NodeStatusReady}, nil
}

func (service *testHeartbeater) ReportAssignment(_ context.Context, nodeID string, update *xirangv1.AssignmentUpdate) error {
	service.assignmentNodeID = nodeID
	service.assignment = update
	return nil
}

func (service *testHeartbeater) ReportAssignmentLog(_ context.Context, nodeID string, snapshot *xirangv1.AssignmentLogSnapshot) error {
	service.assignmentNodeID = nodeID
	service.assignmentLog = snapshot
	return nil
}

func (service *testHeartbeater) HeartbeatWithMode(ctx context.Context, id, serial string, mode domain.NodeStatus) (domain.Node, error) {
	if mode != "" {
		service.lastMode = mode
	}
	return service.Heartbeat(ctx, id, serial)
}

func (service *testHeartbeater) Heartbeat(_ context.Context, id, serial string) (domain.Node, error) {
	service.calls++
	service.serial = serial
	status := service.status
	if status == "" {
		status = domain.NodeStatusReady
	}
	return domain.Node{ID: id, Status: status, Capacity: service.capacity}, nil
}

type testTelemetryReporter struct {
	inventoryNodeID      string
	inventory            *xirangv1.InventorySnapshot
	observationNodeID    string
	observationMessageID string
	observation          *xirangv1.ResourceObservation
}

type testTopologyReporter struct {
	nodeID      string
	messageID   string
	observation *xirangv1.TopologyObservation
}

type testFlowReporter struct {
	nodeID      string
	messageID   string
	observation *xirangv1.FlowObservation
}

func (reporter *testFlowReporter) Report(_ context.Context, nodeID, messageID string, _ time.Time, observation *xirangv1.FlowObservation) error {
	reporter.nodeID, reporter.messageID, reporter.observation = nodeID, messageID, observation
	return nil
}

func (reporter *testTopologyReporter) Report(_ context.Context, nodeID, messageID string, _ time.Time, observation *xirangv1.TopologyObservation) (domain.TopologySnapshot, bool, error) {
	reporter.nodeID, reporter.messageID, reporter.observation = nodeID, messageID, observation
	return domain.TopologySnapshot{Version: 1}, true, nil
}

func (reporter *testTelemetryReporter) ReportInventory(_ context.Context, nodeID string, _ time.Time, inventory *xirangv1.InventorySnapshot) error {
	reporter.inventoryNodeID = nodeID
	reporter.inventory = inventory
	return nil
}

func (reporter *testTelemetryReporter) ReportObservation(_ context.Context, nodeID, messageID string, _ time.Time, observation *xirangv1.ResourceObservation) error {
	reporter.observationNodeID = nodeID
	reporter.observationMessageID = messageID
	reporter.observation = observation
	return nil
}

func (service *testHeartbeater) ConfirmNodeMode(_ context.Context, id, _ string, accepted bool, _ string) (domain.Node, error) {
	status := domain.NodeStatusCordoned
	if !accepted {
		status = domain.NodeStatusReady
	}
	return domain.Node{ID: id, Status: status}, nil
}

type testStream struct {
	ctx      context.Context
	received []*xirangv1.AgentMessage
	sent     []*xirangv1.ControlMessage
}

type asyncTestStream struct {
	ctx      context.Context
	incoming chan *xirangv1.AgentMessage
	outgoing chan *xirangv1.ControlMessage
}

func newTestStream(nodeID string, messages []*xirangv1.AgentMessage) *testStream {
	return &testStream{ctx: testPeerContext(nodeID), received: messages}
}

func newAsyncTestStream(nodeID string) *asyncTestStream {
	return &asyncTestStream{
		ctx: testPeerContext(nodeID), incoming: make(chan *xirangv1.AgentMessage, 4),
		outgoing: make(chan *xirangv1.ControlMessage, 4),
	}
}

func testPeerContext(nodeID string) context.Context {
	nodeURI, _ := url.Parse("spiffe://xirang/node/" + nodeID)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(42), URIs: []*url.URL{nodeURI}}
	tlsInfo := credentials.TLSInfo{State: tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certificate},
		VerifiedChains:   [][]*x509.Certificate{{certificate}},
	}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: tlsInfo})
}

func (stream *testStream) Send(message *xirangv1.ControlMessage) error {
	stream.sent = append(stream.sent, message)
	return nil
}

func (stream *testStream) Recv() (*xirangv1.AgentMessage, error) {
	if len(stream.received) == 0 {
		return nil, io.EOF
	}
	message := stream.received[0]
	stream.received = stream.received[1:]
	return message, nil
}

func (stream *testStream) SetHeader(metadata.MD) error  { return nil }
func (stream *testStream) SendHeader(metadata.MD) error { return nil }
func (stream *testStream) SetTrailer(metadata.MD)       {}
func (stream *testStream) Context() context.Context     { return stream.ctx }
func (stream *testStream) SendMsg(any) error            { return nil }
func (stream *testStream) RecvMsg(any) error            { return nil }

func (stream *asyncTestStream) Send(message *xirangv1.ControlMessage) error {
	stream.outgoing <- message
	return nil
}

func (stream *asyncTestStream) Recv() (*xirangv1.AgentMessage, error) {
	message, ok := <-stream.incoming
	if !ok {
		return nil, io.EOF
	}
	return message, nil
}

func (stream *asyncTestStream) SetHeader(metadata.MD) error  { return nil }
func (stream *asyncTestStream) SendHeader(metadata.MD) error { return nil }
func (stream *asyncTestStream) SetTrailer(metadata.MD)       {}
func (stream *asyncTestStream) Context() context.Context     { return stream.ctx }
func (stream *asyncTestStream) SendMsg(any) error            { return nil }
func (stream *asyncTestStream) RecvMsg(any) error            { return nil }

func helloMessage(nodeID string) *xirangv1.AgentMessage {
	return &xirangv1.AgentMessage{MessageId: "hello-1", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_Hello{Hello: &xirangv1.AgentHello{
		NodeId: nodeID, AgentVersion: "test-version", ProtocolVersion: "v1", Capabilities: []string{heartbeatCapability, "unknown-v1"},
	}}}
}

func heartbeatMessage(sequence uint64) *xirangv1.AgentMessage {
	return &xirangv1.AgentMessage{MessageId: "heartbeat", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_Heartbeat{Heartbeat: &xirangv1.Heartbeat{
		Sequence: sequence, Mode: xirangv1.NodeMode_NODE_MODE_READY,
	}}}
}
