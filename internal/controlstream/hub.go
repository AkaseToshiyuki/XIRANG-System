package controlstream

import (
	"errors"
	"sync"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var ErrCommandQueueFull = errors.New("node command queue is full")

const defaultCommandQueueCapacity = 128

type Hub struct {
	mu       sync.Mutex
	capacity int
	nodes    map[string]*nodeCommands
}

type nodeCommands struct {
	queued   []*commandRecord
	inFlight map[string]*commandRecord
	active   *Session
}

type commandRecord struct {
	message *xirangv1.ControlMessage
	owner   *Session
}

type Session struct {
	hub        *Hub
	nodeID     string
	wake       chan struct{}
	replaced   chan struct{}
	authorized func() bool
}

func NewHub(capacity int) *Hub {
	if capacity <= 0 {
		capacity = defaultCommandQueueCapacity
	}
	return &Hub{capacity: capacity, nodes: make(map[string]*nodeCommands)}
}

func (hub *Hub) RestorePersistentCommands(nodes []domain.Node) error {
	for _, node := range nodes {
		if node.PendingModeCommandID != "" || node.DesiredStatus != "" {
			var mode xirangv1.NodeMode
			switch node.DesiredStatus {
			case domain.NodeStatusReady:
				mode = xirangv1.NodeMode_NODE_MODE_READY
			case domain.NodeStatusCordoned:
				mode = xirangv1.NodeMode_NODE_MODE_CORDONED
			case domain.NodeStatusDraining:
				mode = xirangv1.NodeMode_NODE_MODE_DRAINING
			default:
				return errors.New("persisted node command has an invalid desired status")
			}
			if node.PendingModeCommandID == "" {
				return errors.New("persisted node mode command has no command ID")
			}
			if err := hub.Enqueue(node.ID, &xirangv1.ControlMessage{
				MessageId: node.PendingModeCommandID, SentAt: timestamppb.Now(),
				Payload: &xirangv1.ControlMessage_SetNodeMode{SetNodeMode: &xirangv1.SetNodeMode{Mode: mode, Reason: "restored after controller restart"}},
			}); err != nil {
				return err
			}
		}
		if node.PendingCertificateCommandID != "" || node.PendingCertificateRotationID != "" {
			if node.PendingCertificateCommandID == "" || node.PendingCertificateRotationID == "" {
				return errors.New("persisted certificate rotation command is incomplete")
			}
			if err := hub.Enqueue(node.ID, &xirangv1.ControlMessage{
				MessageId: node.PendingCertificateCommandID, SentAt: timestamppb.Now(),
				Payload: &xirangv1.ControlMessage_RotateCertificate{RotateCertificate: &xirangv1.RotateCertificate{
					RotationId: node.PendingCertificateRotationID,
				}},
			}); err != nil {
				return err
			}
		}
		if node.PendingManagementCommandID != "" || node.DesiredManagementPanel != nil {
			if node.PendingManagementCommandID == "" || node.DesiredManagementPanel == nil {
				return errors.New("persisted management panel command is incomplete")
			}
			if err := hub.Enqueue(node.ID, &xirangv1.ControlMessage{
				MessageId: node.PendingManagementCommandID, SentAt: timestamppb.Now(),
				Payload: &xirangv1.ControlMessage_SetManagementPanel{SetManagementPanel: &xirangv1.SetManagementPanel{
					Enabled: *node.DesiredManagementPanel,
				}},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (hub *Hub) RestoreNodeModeCommands(nodes []domain.Node) error {
	return hub.RestorePersistentCommands(nodes)
}

func (hub *Hub) Enqueue(nodeID string, message *xirangv1.ControlMessage) error {
	if nodeID == "" {
		return errors.New("node ID is required")
	}
	if err := validateQueuedControlMessage(message); err != nil {
		return err
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	node := hub.nodeLocked(nodeID)
	if existingRecord(node, message.MessageId) {
		return nil
	}
	if len(node.queued)+len(node.inFlight) >= hub.capacity {
		return ErrCommandQueueFull
	}
	cloned := proto.Clone(message).(*xirangv1.ControlMessage)
	node.queued = append(node.queued, &commandRecord{message: cloned})
	notify(node.active)
	return nil
}

// EnqueueBatch validates capacity and appends every command under one lock. It
// is used by gang scheduling so a queue-capacity failure cannot expose only a
// subset of prepare or release commands.
func (hub *Hub) EnqueueBatch(commands map[string][]*xirangv1.ControlMessage) error {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	type pending struct {
		nodeID  string
		message *xirangv1.ControlMessage
	}
	validated := make([]pending, 0)
	additional := make(map[string]int)
	batchIDs := make(map[string]map[string]bool)
	for nodeID, messages := range commands {
		if nodeID == "" {
			return errors.New("node ID is required")
		}
		node := hub.nodeLocked(nodeID)
		if batchIDs[nodeID] == nil {
			batchIDs[nodeID] = make(map[string]bool)
		}
		for _, message := range messages {
			if err := validateQueuedControlMessage(message); err != nil {
				return err
			}
			if existingRecord(node, message.MessageId) || batchIDs[nodeID][message.MessageId] {
				continue
			}
			batchIDs[nodeID][message.MessageId] = true
			additional[nodeID]++
			validated = append(validated, pending{nodeID: nodeID, message: message})
		}
	}
	for nodeID, count := range additional {
		node := hub.nodeLocked(nodeID)
		if len(node.queued)+len(node.inFlight)+count > hub.capacity {
			return ErrCommandQueueFull
		}
	}
	for _, command := range validated {
		node := hub.nodeLocked(command.nodeID)
		node.queued = append(node.queued, &commandRecord{message: proto.Clone(command.message).(*xirangv1.ControlMessage)})
	}
	for nodeID := range additional {
		notify(hub.nodeLocked(nodeID).active)
	}
	return nil
}

func validateQueuedControlMessage(message *xirangv1.ControlMessage) error {
	if message == nil || message.MessageId == "" || message.SentAt == nil {
		return errors.New("command message ID and timestamp are required")
	}
	if message.GetRunAssignment() == nil && message.GetCancelAssignment() == nil && message.GetReleaseAssignment() == nil &&
		message.GetSetAssignmentStoragePolicy() == nil && message.GetSetNodeMode() == nil && message.GetRotateCertificate() == nil &&
		message.GetSetTelemetryInterval() == nil && message.GetUpdateNetworkProbeTargets() == nil &&
		message.GetOpenTerminal() == nil && message.GetTerminalInput() == nil && message.GetResizeTerminal() == nil && message.GetCloseTerminal() == nil &&
		message.GetRunNativeBuild() == nil && message.GetSetManagementPanel() == nil && message.GetSetBmcIdentify() == nil {
		return errors.New("control message does not contain a command")
	}
	return nil
}

func (hub *Hub) NodeActive(nodeID string) bool {
	hub.mu.Lock()
	node := hub.nodes[nodeID]
	var session *Session
	if node != nil {
		session = node.active
	}
	hub.mu.Unlock()
	if session == nil || !session.Authorized() {
		return false
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.nodes[nodeID].active == session
}

func (hub *Hub) ActiveNodeIDs() []string {
	hub.mu.Lock()
	ids := make([]string, 0, len(hub.nodes))
	for id, node := range hub.nodes {
		if node.active != nil {
			ids = append(ids, id)
		}
	}
	hub.mu.Unlock()
	active := ids[:0]
	for _, id := range ids {
		if hub.NodeActive(id) {
			active = append(active, id)
		}
	}
	return active
}

func (hub *Hub) Attach(nodeID string) *Session {
	return hub.attachAuthorized(nodeID, nil)
}

func (hub *Hub) attachAuthorized(nodeID string, authorized func() bool) *Session {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	node := hub.nodeLocked(nodeID)
	if node.active != nil {
		close(node.active.replaced)
		hub.requeueInFlightLocked(node)
	}
	session := &Session{hub: hub, nodeID: nodeID, wake: make(chan struct{}, 1), replaced: make(chan struct{}), authorized: authorized}
	node.active = session
	if len(node.queued) > 0 {
		notify(session)
	}
	return session
}

func (session *Session) Detach() bool {
	session.hub.mu.Lock()
	defer session.hub.mu.Unlock()
	node := session.hub.nodeLocked(session.nodeID)
	if node.active != session {
		return false
	}
	session.hub.requeueInFlightLocked(node)
	node.active = nil
	return true
}

func (session *Session) Wake() <-chan struct{}     { return session.wake }
func (session *Session) Replaced() <-chan struct{} { return session.replaced }

func (session *Session) Authorized() bool { return session.authorized == nil || session.authorized() }

func (session *Session) Take() (*xirangv1.ControlMessage, bool) {
	if !session.Authorized() {
		return nil, false
	}
	session.hub.mu.Lock()
	defer session.hub.mu.Unlock()
	node := session.hub.nodeLocked(session.nodeID)
	if node.active != session || len(node.queued) == 0 || len(node.inFlight) > 0 {
		return nil, false
	}
	record := node.queued[0]
	node.queued = node.queued[1:]
	record.owner = session
	node.inFlight[record.message.MessageId] = record
	if len(node.queued) > 0 {
		notify(session)
	}
	return record.message, true
}

func (session *Session) Requeue(messageID string) {
	session.hub.mu.Lock()
	defer session.hub.mu.Unlock()
	node := session.hub.nodeLocked(session.nodeID)
	record, ok := node.inFlight[messageID]
	if !ok {
		return
	}
	delete(node.inFlight, messageID)
	record.owner = nil
	node.queued = append([]*commandRecord{record}, node.queued...)
	notify(node.active)
}

func (session *Session) Acknowledge(messageID string) (*xirangv1.ControlMessage, bool) {
	session.hub.mu.Lock()
	defer session.hub.mu.Unlock()
	node := session.hub.nodeLocked(session.nodeID)
	record, ok := node.inFlight[messageID]
	if !ok || record.owner != session {
		return nil, false
	}
	delete(node.inFlight, messageID)
	record.owner = nil
	if len(node.queued) > 0 {
		notify(node.active)
	}
	return record.message, true
}

func (hub *Hub) nodeLocked(nodeID string) *nodeCommands {
	node := hub.nodes[nodeID]
	if node == nil {
		node = &nodeCommands{inFlight: make(map[string]*commandRecord)}
		hub.nodes[nodeID] = node
	}
	return node
}

func (hub *Hub) requeueInFlightLocked(node *nodeCommands) {
	if len(node.inFlight) == 0 {
		return
	}
	requeued := make([]*commandRecord, 0, len(node.inFlight)+len(node.queued))
	for _, record := range node.inFlight {
		record.owner = nil
		requeued = append(requeued, record)
	}
	node.queued = append(requeued, node.queued...)
	clear(node.inFlight)
}

func existingRecord(node *nodeCommands, messageID string) bool {
	if _, ok := node.inFlight[messageID]; ok {
		return true
	}
	for _, record := range node.queued {
		if record.message.MessageId == messageID {
			return true
		}
	}
	return false
}

func notify(session *Session) {
	if session == nil {
		return
	}
	select {
	case session.wake <- struct{}{}:
	default:
	}
}
