package terminal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
)

var ErrUnavailable = errors.New("terminal unavailable")

const (
	terminalTicketTTL       = 30 * time.Second
	terminalIdleTimeout     = 10 * time.Minute
	terminalSessionDuration = 30 * time.Minute
)

type NodeReader interface {
	GetNode(context.Context, string) (domain.Node, error)
}

type AuditWriter interface {
	AppendAuditEvent(context.Context, domain.AuditEvent) error
}

type ticket struct {
	node      domain.Node
	actor     domain.Principal
	expiresAt time.Time
}

type Service struct {
	nodes    NodeReader
	broker   *Broker
	audits   AuditWriter
	mu       sync.Mutex
	tickets  map[string]ticket
	active   map[string]activeSession
	now      func() time.Time
	upgrader websocket.Upgrader
}

type activeSession struct {
	userID string
	cancel context.CancelFunc
}

func New(nodes NodeReader, broker *Broker, audits AuditWriter) (*Service, error) {
	if nodes == nil || broker == nil {
		return nil, ErrUnavailable
	}
	return &Service{
		nodes: nodes, broker: broker, audits: audits, tickets: make(map[string]ticket), active: make(map[string]activeSession), now: time.Now,
		upgrader: websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, CheckOrigin: sameOrigin},
	}, nil
}

func (service *Service) Issue(ctx context.Context, nodeID string, principals ...domain.Principal) (string, time.Time, error) {
	node, err := service.nodes.GetNode(ctx, nodeID)
	if err != nil {
		return "", time.Time{}, err
	}
	if !service.broker.transport.NodeActive(node.ID) {
		return "", time.Time{}, ErrUnavailable
	}
	actor := domain.Principal{Subject: "bootstrap-admin", DisplayName: "Bootstrap administrator", Role: "admin", AuthMethod: "bootstrap"}
	if len(principals) > 0 {
		actor = principals[0]
	}
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", time.Time{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(buffer)
	expiresAt := service.now().UTC().Add(terminalTicketTTL)
	service.mu.Lock()
	for id, current := range service.tickets {
		if current.expiresAt.Before(service.now()) {
			delete(service.tickets, id)
		}
	}
	service.tickets[secret] = ticket{node: node, actor: actor, expiresAt: expiresAt}
	service.mu.Unlock()
	return secret, expiresAt, nil
}

func (service *Service) Connect(writer http.ResponseWriter, request *http.Request) {
	secret := request.URL.Query().Get("ticket")
	service.mu.Lock()
	current, ok := service.tickets[secret]
	delete(service.tickets, secret)
	service.mu.Unlock()
	if !ok || current.expiresAt.Before(service.now()) {
		http.Error(writer, "terminal ticket expired", http.StatusUnauthorized)
		return
	}
	connection, err := service.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(maxTerminalInputFrame)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	session, err := service.broker.Open(ctx, current.node.ID, 120, 36)
	if err != nil {
		_ = connection.WriteMessage(websocket.TextMessage, []byte("Unable to open Agent PTY\r\n"))
		service.audit(ctx, current, "terminal.session.open", "rejected", "", 0, 0, err.Error())
		return
	}
	service.audit(context.Background(), current, "terminal.session.open", "success", session.ID, 0, 0, "")
	userID := current.actor.UserID
	if userID == "" {
		userID = current.actor.Subject
	}
	service.mu.Lock()
	service.active[session.ID] = activeSession{userID: userID, cancel: cancel}
	service.mu.Unlock()
	defer func() {
		service.mu.Lock()
		delete(service.active, session.ID)
		service.mu.Unlock()
	}()
	defer session.Close("browser disconnected")

	type inputFrame struct {
		data []byte
		err  error
	}
	input := make(chan inputFrame, 1)
	go func() {
		for {
			_, payload, readErr := connection.ReadMessage()
			select {
			case input <- inputFrame{data: payload, err: readErr}:
			case <-ctx.Done():
				return
			}
			if readErr != nil {
				return
			}
		}
	}()
	idle := time.NewTimer(terminalIdleTimeout)
	defer idle.Stop()
	total := time.NewTimer(terminalSessionDuration)
	defer total.Stop()
	var inputBytes, outputBytes uint64
	closeReason := "browser disconnected"
	for {
		select {
		case frame := <-input:
			if frame.err != nil {
				goto finished
			}
			inputBytes += uint64(len(frame.data))
			if err := session.Input(frame.data); err != nil {
				closeReason = err.Error()
				goto finished
			}
			resetTimer(idle, terminalIdleTimeout)
		case event, open := <-session.Events:
			if !open {
				closeReason = "Agent session closed"
				goto finished
			}
			switch event.Type {
			case xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_OUTPUT:
				outputBytes += uint64(len(event.Data))
				if err := connection.WriteMessage(websocket.BinaryMessage, event.Data); err != nil {
					closeReason = "browser output disconnected"
					goto finished
				}
				resetTimer(idle, terminalIdleTimeout)
			case xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_CLOSED, xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_ERROR:
				closeReason = strings.TrimSpace(event.Message)
				if closeReason == "" {
					closeReason = "Agent session closed"
				}
				goto finished
			}
		case <-idle.C:
			closeReason = "idle timeout"
			goto finished
		case <-total.C:
			closeReason = "maximum session duration reached"
			goto finished
		case <-ctx.Done():
			goto finished
		}
	}

finished:
	_ = session.Close(closeReason)
	service.audit(context.Background(), current, "terminal.session.close", "success", session.ID, inputBytes, outputBytes, closeReason)
}

// TerminateUsers invalidates unused terminal tickets and closes active PTYs for
// the supplied local user IDs. It returns the number of affected sessions.
func (service *Service) TerminateUsers(userIDs []string) int {
	wanted := make(map[string]struct{}, len(userIDs))
	for _, id := range userIDs {
		wanted[id] = struct{}{}
	}
	service.mu.Lock()
	cancels := make([]context.CancelFunc, 0)
	for secret, current := range service.tickets {
		id := current.actor.UserID
		if id == "" {
			id = current.actor.Subject
		}
		if _, ok := wanted[id]; ok {
			delete(service.tickets, secret)
		}
	}
	for _, current := range service.active {
		if _, ok := wanted[current.userID]; ok {
			cancels = append(cancels, current.cancel)
		}
	}
	service.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

func (service *Service) audit(ctx context.Context, current ticket, action, outcome, sessionID string, inputBytes, outputBytes uint64, reason string) {
	if service.audits == nil {
		return
	}
	id, err := terminalID()
	if err != nil {
		return
	}
	details := map[string]string{
		"sessionId": sessionID, "nodeName": current.node.Name,
		"inputBytes": uintToString(inputBytes), "outputBytes": uintToString(outputBytes),
	}
	if reason != "" {
		details["reason"] = reason
	}
	actorID := current.actor.Subject
	if current.actor.UserID != "" {
		actorID = current.actor.UserID
	}
	_ = service.audits.AppendAuditEvent(ctx, domain.AuditEvent{
		ID: id, ActorType: "user", ActorID: actorID, Action: action,
		TargetKind: "node", TargetID: current.node.ID, Outcome: outcome,
		OccurredAt: service.now().UTC(), Details: details,
	})
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func uintToString(value uint64) string {
	if value == 0 {
		return "0"
	}
	buffer := make([]byte, 0, 20)
	for value > 0 {
		buffer = append(buffer, byte('0'+value%10))
		value /= 10
	}
	for left, right := 0, len(buffer)-1; left < right; left, right = left+1, right-1 {
		buffer[left], buffer[right] = buffer[right], buffer[left]
	}
	return string(buffer)
}

func sameOrigin(request *http.Request) bool {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, request.Host) {
		return false
	}
	if request.TLS != nil {
		return strings.EqualFold(parsed.Scheme, "https")
	}
	return strings.EqualFold(parsed.Scheme, "http")
}
