package agent

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

type idleControlServer struct {
	xirangv1.UnimplementedAgentControlServer
}

func (idleControlServer) Connect(stream grpc.BidiStreamingServer[xirangv1.AgentMessage, xirangv1.ControlMessage]) error {
	<-stream.Context().Done()
	return nil
}

func startControlServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	xirangv1.RegisterAgentControlServer(server, idleControlServer{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// reservedUnreachableAddress returns an address that accepts no connections.
// Binding and closing a listener yields a port nothing is listening on, which
// produces a refusal rather than a hang.
func reservedUnreachableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// TestDialControlPlaneReturnsAReadyConnection pins the property the retry loop
// depends on. grpc.NewClient connects lazily, so a connection handed back
// before it is usable would push dial failures into the session instead of the
// backoff that is meant to absorb them.
func TestDialControlPlaneReturnsAReadyConnection(t *testing.T) {
	address := startControlServer(t)
	connection, err := dialControlPlane(context.Background(), address, controlDialTimeout, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling a reachable control plane failed: %v", err)
	}
	defer connection.Close()
	if state := connection.GetState(); state != connectivity.Ready {
		t.Fatalf("dial returned a connection in state %s, want %s", state, connectivity.Ready)
	}
	stream, err := xirangv1.NewAgentControlClient(connection).Connect(context.Background())
	if err != nil {
		t.Fatalf("the returned connection could not open a stream: %v", err)
	}
	_ = stream.CloseSend()
}

// TestDialControlPlaneReportsWhyAConnectionFailed is the reason this code does
// more than wait for a state. The connectivity API reports only that a
// connection is not ready, which cannot tell a refused port from a rejected
// certificate, and that distinction is what an operator reads out of the
// Agent's log when a node will not attach.
func TestDialControlPlaneReportsWhyAConnectionFailed(t *testing.T) {
	address := reservedUnreachableAddress(t)
	// A refused port is retried by gRPC until the budget runs out, which gives
	// a restarting controller a chance to answer within one attempt. The budget
	// is passed in rather than the caller's context being shortened, so this
	// exercises what production does: a live context and an expired dial
	// deadline, which is the only combination where the cause can be recovered.
	const budget = 2 * time.Second
	started := time.Now()
	connection, err := dialControlPlane(context.Background(), address, budget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err == nil {
		connection.Close()
		t.Fatal("dialling an address with no listener succeeded")
	}
	if elapsed := time.Since(started); elapsed > budget+controlDialCauseTimeout+time.Second {
		t.Fatalf("a failed dial took %s, well past its %s budget", elapsed, budget)
	}
	if !strings.Contains(err.Error(), "connect control stream") {
		t.Fatalf("the error lost its context: %v", err)
	}
	// The refusal itself has to survive into the message; "not ready" alone
	// would leave an operator guessing.
	if !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "connection error") {
		t.Fatalf("the error does not say why the connection failed: %v", err)
	}
}

// TestDialControlPlaneStopsWithItsContext keeps shutdown responsive: an Agent
// asked to stop while the controller is unreachable must not sit out the full
// dial budget first.
func TestDialControlPlaneStopsWithItsContext(t *testing.T) {
	address := reservedUnreachableAddress(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	connection, err := dialControlPlane(ctx, address, controlDialTimeout, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err == nil {
		connection.Close()
		t.Fatal("dialling with a cancelled context succeeded")
	}
	if elapsed := time.Since(started); elapsed > controlDialTimeout {
		t.Fatalf("a cancelled dial took %s; it should not wait out the %s budget", elapsed, controlDialTimeout)
	}
}
