package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxTerminalSessions        = 16
	maxTerminalSessionsPerNode = 4
	maxTerminalInputFrame      = 64 << 10
)

type CommandTransport interface {
	Enqueue(string, *xirangv1.ControlMessage) error
	NodeActive(string) bool
}

type Event struct {
	Type     xirangv1.TerminalEventType
	Data     []byte
	Message  string
	ExitCode int32
	At       time.Time
}

type Session struct {
	ID     string
	NodeID string
	Events <-chan Event
	broker *Broker
}

func (session *Session) Input(data []byte) error { return session.broker.Input(session.ID, data) }
func (session *Session) Resize(columns, rows uint32) error {
	return session.broker.Resize(session.ID, columns, rows)
}
func (session *Session) Close(reason string) error { return session.broker.Close(session.ID, reason) }

type brokerSession struct {
	nodeID string
	events chan Event
}

// Broker multiplexes ephemeral PTY frames over the existing Agent mTLS stream.
// It intentionally stores no terminal payload and does not survive controller
// restart; a disconnected control plane always tears down the PTY.
type Broker struct {
	transport CommandTransport
	mu        sync.Mutex
	sessions  map[string]*brokerSession
}

func NewBroker(transport CommandTransport) *Broker {
	return &Broker{transport: transport, sessions: make(map[string]*brokerSession)}
}

func (broker *Broker) Open(_ context.Context, nodeID string, columns, rows uint32) (*Session, error) {
	if broker == nil || broker.transport == nil || nodeID == "" || !broker.transport.NodeActive(nodeID) {
		return nil, ErrUnavailable
	}
	if columns < 20 || columns > 500 || rows < 5 || rows > 300 {
		return nil, errors.New("terminal dimensions are outside safe limits")
	}
	id, err := terminalID()
	if err != nil {
		return nil, err
	}
	broker.mu.Lock()
	if len(broker.sessions) >= maxTerminalSessions {
		broker.mu.Unlock()
		return nil, errors.New("terminal session limit reached")
	}
	perNode := 0
	for _, current := range broker.sessions {
		if current.nodeID == nodeID {
			perNode++
		}
	}
	if perNode >= maxTerminalSessionsPerNode {
		broker.mu.Unlock()
		return nil, errors.New("node terminal session limit reached")
	}
	events := make(chan Event, 128)
	broker.sessions[id] = &brokerSession{nodeID: nodeID, events: events}
	broker.mu.Unlock()
	message := newControlMessage()
	message.Payload = &xirangv1.ControlMessage_OpenTerminal{OpenTerminal: &xirangv1.OpenTerminal{SessionId: id, Columns: columns, Rows: rows}}
	if err := broker.transport.Enqueue(nodeID, message); err != nil {
		broker.remove(id)
		return nil, err
	}
	return &Session{ID: id, NodeID: nodeID, Events: events, broker: broker}, nil
}

func (broker *Broker) Input(id string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxTerminalInputFrame {
		return errors.New("terminal input frame exceeds 64 KiB")
	}
	nodeID, ok := broker.node(id)
	if !ok {
		return ErrUnavailable
	}
	cloned := append([]byte(nil), data...)
	message := newControlMessage()
	message.Payload = &xirangv1.ControlMessage_TerminalInput{TerminalInput: &xirangv1.TerminalInput{SessionId: id, Data: cloned}}
	return broker.transport.Enqueue(nodeID, message)
}

func (broker *Broker) Resize(id string, columns, rows uint32) error {
	if columns < 20 || columns > 500 || rows < 5 || rows > 300 {
		return errors.New("terminal dimensions are outside safe limits")
	}
	nodeID, ok := broker.node(id)
	if !ok {
		return ErrUnavailable
	}
	message := newControlMessage()
	message.Payload = &xirangv1.ControlMessage_ResizeTerminal{ResizeTerminal: &xirangv1.ResizeTerminal{SessionId: id, Columns: columns, Rows: rows}}
	return broker.transport.Enqueue(nodeID, message)
}

func (broker *Broker) Close(id, reason string) error {
	nodeID, ok := broker.node(id)
	if !ok {
		return nil
	}
	if len(reason) > 256 {
		reason = reason[:256]
	}
	message := newControlMessage()
	message.Payload = &xirangv1.ControlMessage_CloseTerminal{CloseTerminal: &xirangv1.CloseTerminal{SessionId: id, Reason: reason}}
	err := broker.transport.Enqueue(nodeID, message)
	broker.remove(id)
	return err
}

func (broker *Broker) ReportTerminalEvent(_ context.Context, nodeID string, event *xirangv1.TerminalEvent) error {
	if event == nil || event.SessionId == "" || event.Type == xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_UNSPECIFIED || len(event.Data) > 64<<10 {
		return errors.New("invalid terminal event")
	}
	broker.mu.Lock()
	current := broker.sessions[event.SessionId]
	if current == nil || current.nodeID != nodeID {
		broker.mu.Unlock()
		return nil
	}
	at := time.Now().UTC()
	if event.OccurredAt != nil && event.OccurredAt.IsValid() {
		at = event.OccurredAt.AsTime()
	}
	forward := Event{Type: event.Type, Data: append([]byte(nil), event.Data...), Message: event.Message, ExitCode: event.ExitCode, At: at}
	select {
	case current.events <- forward:
	default:
		broker.mu.Unlock()
		_ = broker.Close(event.SessionId, "controller output buffer exceeded")
		// Overflow closes only the offending PTY. It is a per-session backpressure
		// event and must not tear down the Agent's shared control stream.
		return nil
	}
	closed := event.Type == xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_CLOSED || event.Type == xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_ERROR
	if closed {
		delete(broker.sessions, event.SessionId)
		close(current.events)
	}
	broker.mu.Unlock()
	return nil
}

func (broker *Broker) node(id string) (string, bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	current := broker.sessions[id]
	if current == nil {
		return "", false
	}
	return current.nodeID, true
}

func (broker *Broker) remove(id string) {
	broker.mu.Lock()
	current := broker.sessions[id]
	if current != nil {
		delete(broker.sessions, id)
		close(current.events)
	}
	broker.mu.Unlock()
}

func newControlMessage() *xirangv1.ControlMessage {
	return &xirangv1.ControlMessage{MessageId: "terminal-command-" + randomHex(16), SentAt: timestamppb.Now()}
}

func terminalID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "terminal-" + hex.EncodeToString(buffer), nil
}

func randomHex(size int) string {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().String()))[:size*2]
	}
	return hex.EncodeToString(buffer)
}
