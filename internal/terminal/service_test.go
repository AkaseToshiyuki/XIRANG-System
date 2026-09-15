package terminal

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
)

type fixedNodes struct{ node domain.Node }

func (nodes fixedNodes) GetNode(context.Context, string) (domain.Node, error) { return nodes.node, nil }

type fixedTransport struct {
	active   bool
	messages []*xirangv1.ControlMessage
}

func (transport *fixedTransport) NodeActive(string) bool { return transport.active }
func (transport *fixedTransport) Enqueue(_ string, message *xirangv1.ControlMessage) error {
	transport.messages = append(transport.messages, message)
	return nil
}

func TestIssueTerminalTicketRequiresActiveAgentControlStream(t *testing.T) {
	transport := &fixedTransport{}
	service, err := New(fixedNodes{node: domain.Node{ID: "node-1"}}, NewBroker(transport), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Issue(context.Background(), "node-1"); err == nil {
		t.Fatal("ticket was issued without an active Agent stream")
	}
	transport.active = true
	first, expiresAt, err := service.Issue(context.Background(), "node-1")
	if err != nil || first == "" || !expiresAt.After(time.Now()) {
		t.Fatalf("ticket was not issued: ticket=%q expiry=%s err=%v", first, expiresAt, err)
	}
	second, _, err := service.Issue(context.Background(), "node-1")
	if err != nil || first == second {
		t.Fatal("terminal tickets must be independently random")
	}
}

func TestBrokerMultiplexesTerminalCommandsAndEvents(t *testing.T) {
	transport := &fixedTransport{active: true}
	broker := NewBroker(transport)
	session, err := broker.Open(context.Background(), "node-1", 120, 36)
	if err != nil || transport.messages[0].GetOpenTerminal() == nil {
		t.Fatalf("PTY open was not enqueued: session=%#v messages=%#v err=%v", session, transport.messages, err)
	}
	if err := session.Input([]byte("hostname\n")); err != nil || transport.messages[1].GetTerminalInput() == nil {
		t.Fatalf("PTY input was not enqueued: %v", err)
	}
	if err := broker.ReportTerminalEvent(context.Background(), "node-1", &xirangv1.TerminalEvent{
		SessionId: session.ID, Type: xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_OUTPUT, Data: []byte("node-1\r\n"),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-session.Events:
		if string(event.Data) != "node-1\r\n" {
			t.Fatalf("unexpected PTY output: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("PTY output was not delivered")
	}
	if err := session.Close("test complete"); err != nil || transport.messages[len(transport.messages)-1].GetCloseTerminal() == nil {
		t.Fatalf("PTY close was not enqueued: %v", err)
	}
}

func TestTerminalWebSocketOriginMustMatchRequestHostAndScheme(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://control.example/api/v1/terminal/connect", nil)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Origin", "https://control.example")
	if !sameOrigin(request) {
		t.Fatal("same HTTPS origin was rejected")
	}
	request.Header.Set("Origin", "https://attacker.example")
	if sameOrigin(request) {
		t.Fatal("cross-origin WebSocket request was accepted")
	}
	request.Header.Set("Origin", "http://control.example")
	if sameOrigin(request) {
		t.Fatal("mixed-scheme WebSocket request was accepted")
	}
	request.Header.Del("Origin")
	if !sameOrigin(request) {
		t.Fatal("non-browser client without Origin was rejected")
	}
}
