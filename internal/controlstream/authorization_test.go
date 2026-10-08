package controlstream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type authorizationNodes struct {
	state      atomic.Int32
	heartbeats atomic.Int32
}

func (nodes *authorizationNodes) Heartbeat(ctx context.Context, id, _ string) (domain.Node, error) {
	nodes.heartbeats.Add(1)
	return nodes.GetNode(ctx, id)
}
func (nodes *authorizationNodes) GetNode(_ context.Context, id string) (domain.Node, error) {
	node := domain.Node{ID: id, Status: domain.NodeStatusReady, CertificateSerial: "2a"}
	now := time.Now()
	switch nodes.state.Load() {
	case 1:
		node.CertificateRevokedAt = &now
	case 2:
		node.CertificateSerial = "replacement"
	case 3:
		node.RemovedAt = &now
	}
	return node, nil
}

func startAuthorizationStream(t *testing.T, interval time.Duration, expiry ...time.Duration) (*authorizationNodes, *Hub, *Server, *asyncTestStream, <-chan error) {
	t.Helper()
	nodes, hub := &authorizationNodes{}, NewHub(8)
	server := New(nodes, time.Second, hub)
	server.SetNativeBuildReporter(buildmanager.NewRemotePool(hub))
	server.heartbeatInterval = interval
	server.heartbeatTimeout = 3 * interval
	stream := newAsyncTestStream("node_test")
	if len(expiry) > 0 {
		_, certificate, err := authenticatedNode(stream.ctx)
		if err != nil {
			t.Fatal(err)
		}
		certificate.NotAfter = time.Now().Add(expiry[0])
	}
	ctx, cancel := context.WithCancel(stream.ctx)
	stream.ctx = ctx
	t.Cleanup(func() { cancel(); close(stream.incoming) })
	done := make(chan error, 1)
	go func() { done <- server.Connect(stream) }()
	hello := helloMessage("node_test")
	hello.GetHello().Capabilities = append(hello.GetHello().Capabilities, remoteNativeBuildCapability)
	stream.incoming <- hello
	select {
	case reply := <-stream.outgoing:
		if reply.GetHelloAccepted() == nil {
			t.Fatal("hello failed")
		}
	case <-time.After(time.Second):
		t.Fatal("hello timeout")
	}
	deadline := time.Now().Add(time.Second)
	for !hub.NodeActive("node_test") {
		if time.Now().After(deadline) {
			t.Fatal("session did not attach")
		}
		time.Sleep(time.Millisecond)
	}
	return nodes, hub, server, stream, done
}

func TestRevocationDisablesRemoteWorkerWithoutHeartbeat(t *testing.T) {
	nodes, hub, server, _, done := startAuthorizationStream(t, time.Second)
	pool := server.builds.(*buildmanager.RemotePool)
	if err := pool.ReportWorker("node_test", &xirangv1.NativeBuildWorker{Architecture: "amd64", Toolchains: []string{"go"}, Ready: true}); err != nil {
		t.Fatal(err)
	}
	recipe := domain.BuildRecipe{Architecture: "amd64", Toolchain: "go"}
	if _, ok := pool.Select(recipe); !ok {
		t.Fatal("valid worker not selectable")
	}
	nodes.state.Store(1)
	if hub.NodeActive("node_test") || len(hub.ActiveNodeIDs()) != 0 {
		t.Fatal("revoked session retained authority")
	}
	if _, ok := pool.Select(recipe); ok {
		t.Fatal("revoked worker selectable without heartbeat")
	}
	if nodes.heartbeats.Load() != 1 {
		t.Fatal("read-only checks updated heartbeat state")
	}
	select {
	case err := <-done:
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("revocation result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not close silent stream")
	}
	if len(pool.Workers()) != 0 {
		t.Fatal("revoked worker was not removed")
	}
}

func TestRevokedStreamCannotReceiveCommandsOrReportWorkers(t *testing.T) {
	for _, operation := range []string{"command", "report", "replacement", "removal"} {
		t.Run(operation, func(t *testing.T) {
			nodes, hub, _, stream, done := startAuthorizationStream(t, time.Second)
			nodes.state.Store(1)
			if operation == "replacement" {
				nodes.state.Store(2)
			}
			if operation == "removal" {
				nodes.state.Store(3)
			}
			if operation == "command" {
				if err := hub.Enqueue("node_test", testModeCommand("after-revocation", xirangv1.NodeMode_NODE_MODE_CORDONED)); err != nil {
					t.Fatal(err)
				}
			} else {
				stream.incoming <- &xirangv1.AgentMessage{MessageId: "worker", SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_NativeBuildWorker{NativeBuildWorker: &xirangv1.NativeBuildWorker{Architecture: "amd64", Toolchains: []string{"go"}, Ready: true}}}
			}
			select {
			case err := <-done:
				if status.Code(err) != codes.Unauthenticated {
					t.Fatalf("authorization result: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("revoked operation not rejected")
			}
			select {
			case command := <-stream.outgoing:
				t.Fatalf("delivered revoked command: %v", command)
			default:
			}
		})
	}
}

func TestControlStreamRequiresApplicationHeartbeat(t *testing.T) {
	_, hub, _, _, done := startAuthorizationStream(t, 20*time.Millisecond)
	select {
	case err := <-done:
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("heartbeat timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("silent stream stayed active")
	}
	if hub.NodeActive("node_test") {
		t.Fatal("timed-out stream is active")
	}
}

func TestHeartbeatDeadlineAllowsSynchronousAgentSampling(t *testing.T) {
	server := New(&authorizationNodes{}, time.Second)
	if server.heartbeatTimeout < 30*time.Second {
		t.Fatalf("legal short interval cannot tolerate Agent sampling: %s", server.heartbeatTimeout)
	}
	server = New(&authorizationNodes{}, time.Minute)
	if server.heartbeatTimeout != 3*time.Minute {
		t.Fatalf("long heartbeat interval truncated: %s", server.heartbeatTimeout)
	}
}

func TestExpiredCertificateCannotAttachControlStream(t *testing.T) {
	stream := newTestStream("node_test", []*xirangv1.AgentMessage{helloMessage("node_test")})
	_, certificate, err := authenticatedNode(stream.ctx)
	if err != nil {
		t.Fatal(err)
	}
	certificate.NotAfter = time.Now().Add(-time.Second)
	if err := New(&authorizationNodes{}, time.Second).Connect(stream); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expired cert admitted: %v", err)
	}
}

func TestExistingControlStreamExpiresWithCertificate(t *testing.T) {
	_, hub, _, _, done := startAuthorizationStream(t, time.Second, 50*time.Millisecond)
	select {
	case err := <-done:
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("expired stream: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expired certificate retained existing stream")
	}
	if hub.NodeActive("node_test") {
		t.Fatal("expired stream remains active")
	}
}
