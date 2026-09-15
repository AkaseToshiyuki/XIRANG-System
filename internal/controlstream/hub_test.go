package controlstream

import (
	"errors"
	"testing"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHubBoundsDeduplicatesAndRequeuesUnacknowledgedCommands(t *testing.T) {
	hub := NewHub(2)
	first := testModeCommand("command-1", xirangv1.NodeMode_NODE_MODE_CORDONED)
	second := testModeCommand("command-2", xirangv1.NodeMode_NODE_MODE_DRAINING)
	if err := hub.Enqueue("node_test", first); err != nil {
		t.Fatal(err)
	}
	if err := hub.Enqueue("node_test", first); err != nil {
		t.Fatalf("duplicate enqueue should be idempotent: %v", err)
	}
	if err := hub.Enqueue("node_test", second); err != nil {
		t.Fatal(err)
	}
	if err := hub.Enqueue("node_test", testModeCommand("command-3", xirangv1.NodeMode_NODE_MODE_READY)); !errors.Is(err, ErrCommandQueueFull) {
		t.Fatalf("expected bounded queue error, got %v", err)
	}

	original := hub.Attach("node_test")
	delivered, ok := original.Take()
	if !ok || delivered.MessageId != first.MessageId {
		t.Fatalf("unexpected first delivery: %#v", delivered)
	}
	replacement := hub.Attach("node_test")
	select {
	case <-original.Replaced():
	default:
		t.Fatal("old session was not replaced")
	}
	replayed, ok := replacement.Take()
	if !ok || replayed.MessageId != first.MessageId {
		t.Fatalf("unacknowledged command was not replayed: %#v", replayed)
	}
	if stale, ok := original.Acknowledge(first.MessageId); ok {
		t.Fatalf("replaced session acknowledged a command owned by the new session: %#v", stale)
	}
	acknowledged, ok := replacement.Acknowledge(first.MessageId)
	if !ok || acknowledged.MessageId != first.MessageId {
		t.Fatalf("unexpected acknowledgement result: %#v", acknowledged)
	}
	next, ok := replacement.Take()
	if !ok || next.MessageId != second.MessageId {
		t.Fatalf("unexpected second delivery: %#v", next)
	}
	replacement.Detach()
}

func TestHubRestoresPersistedNodeModeCommand(t *testing.T) {
	hub := NewHub(2)
	if err := hub.RestoreNodeModeCommands([]domain.Node{{
		ID: "node_restore", DesiredStatus: domain.NodeStatusDraining, PendingModeCommandID: "command-restore",
	}}); err != nil {
		t.Fatal(err)
	}
	session := hub.Attach("node_restore")
	defer session.Detach()
	command, ok := session.Take()
	if !ok || command.MessageId != "command-restore" || command.GetSetNodeMode().Mode != xirangv1.NodeMode_NODE_MODE_DRAINING {
		t.Fatalf("unexpected restored command: %#v", command)
	}
}

func TestHubEnqueueBatchIsAtomicOnCapacityFailure(t *testing.T) {
	hub := NewHub(1)
	commands := map[string][]*xirangv1.ControlMessage{
		"node-a": {testModeCommand("a-1", xirangv1.NodeMode_NODE_MODE_READY)},
		"node-b": {
			testModeCommand("b-1", xirangv1.NodeMode_NODE_MODE_READY),
			testModeCommand("b-2", xirangv1.NodeMode_NODE_MODE_READY),
		},
	}
	if err := hub.EnqueueBatch(commands); !errors.Is(err, ErrCommandQueueFull) {
		t.Fatalf("batch capacity failure = %v", err)
	}
	for _, nodeID := range []string{"node-a", "node-b"} {
		session := hub.Attach(nodeID)
		if message, ok := session.Take(); ok || message != nil {
			t.Fatalf("partial command became visible on %s: %#v", nodeID, message)
		}
		session.Detach()
	}
}

func TestHubRestoresPersistedCertificateRotationCommand(t *testing.T) {
	hub := NewHub(2)
	if err := hub.RestorePersistentCommands([]domain.Node{{
		ID: "node_restore", PendingCertificateCommandID: "command-rotate", PendingCertificateRotationID: "rotation-restore",
	}}); err != nil {
		t.Fatal(err)
	}
	session := hub.Attach("node_restore")
	defer session.Detach()
	command, ok := session.Take()
	if !ok || command.MessageId != "command-rotate" || command.GetRotateCertificate().RotationId != "rotation-restore" {
		t.Fatalf("unexpected restored certificate command: %#v", command)
	}
}

func TestHubRestoresPersistedManagementPanelCommand(t *testing.T) {
	hub := NewHub(2)
	desired := true
	if err := hub.RestorePersistentCommands([]domain.Node{{
		ID: "node_management", PendingManagementCommandID: "command-panel", DesiredManagementPanel: &desired,
	}}); err != nil {
		t.Fatal(err)
	}
	session := hub.Attach("node_management")
	defer session.Detach()
	command, ok := session.Take()
	if !ok || command.MessageId != "command-panel" || !command.GetSetManagementPanel().Enabled {
		t.Fatalf("unexpected restored management panel command: %#v", command)
	}
}

func testModeCommand(id string, mode xirangv1.NodeMode) *xirangv1.ControlMessage {
	return &xirangv1.ControlMessage{
		MessageId: id, SentAt: timestamppb.Now(),
		Payload: &xirangv1.ControlMessage_SetNodeMode{SetNodeMode: &xirangv1.SetNodeMode{Mode: mode}},
	}
}
