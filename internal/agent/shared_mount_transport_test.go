package agent

import (
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/controlstream"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"github.com/xirang-grid/xirang/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type mountTransportNodes struct {
	node       domain.Node
	heartbeats atomic.Int32
}

func (nodes *mountTransportNodes) Heartbeat(context.Context, string, string) (domain.Node, error) {
	nodes.heartbeats.Add(1)
	return nodes.node, nil
}
func (nodes *mountTransportNodes) GetNode(context.Context, string) (domain.Node, error) {
	return nodes.node, nil
}

type mountTransportServer struct {
	xirangv1.UnimplementedAgentControlServer
	control      *controlstream.Server
	lostResult   atomic.Bool
	acknowledged chan struct{}
}

func (server *mountTransportServer) Connect(stream grpc.BidiStreamingServer[xirangv1.AgentMessage, xirangv1.ControlMessage]) error {
	return server.control.Connect(&mountTransportStream{stream, server})
}

type mountTransportStream struct {
	grpc.BidiStreamingServer[xirangv1.AgentMessage, xirangv1.ControlMessage]
	server *mountTransportServer
}

func (stream *mountTransportStream) Recv() (*xirangv1.AgentMessage, error) {
	message, err := stream.BidiStreamingServer.Recv()
	if err != nil {
		return nil, err
	}
	if hello := message.GetHello(); hello != nil {
		if !containsCapability(hello.Capabilities, mountcontrol.Capability) {
			return nil, status.Error(codes.InvalidArgument, "missing mount capability")
		}
		// Activate no collector, inventory scan, executor workload or host mutation.
		hello.Capabilities = []string{"heartbeat-v1", mountcontrol.Capability}
	}
	if ack := message.GetAcknowledgement(); ack != nil && ack.Accepted {
		select {
		case stream.server.acknowledged <- struct{}{}:
		default:
		}
	}
	if message.GetSharedMountResult() != nil && stream.server.lostResult.CompareAndSwap(true, false) {
		return nil, status.Error(codes.Unavailable, "injected lost completion after execution")
	}
	return message, nil
}

func TestSharedMountRealMTLSCompletionHeartbeatAndLostResult(t *testing.T) {
	authority, err := pki.OpenOrCreate(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, csr, err := createNodeCSR("mount-test")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := authority.IssueNodeCertificate("node_mount", csr)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	state := State{ServerURL: "https://127.0.0.1", NodeID: "node_mount", Identity: certificate.Serial}
	if err := saveIdentity(directory, state, key, certificate.CertificatePEM, certificate.CACertificatePEM); err != nil {
		t.Fatal(err)
	}
	nodes := &mountTransportNodes{node: domain.Node{ID: state.NodeID, Status: domain.NodeStatusReady, CertificateSerial: certificate.Serial, Capabilities: []string{mountcontrol.Capability}}}
	hub := controlstream.NewHub(8)
	broker := mountcontrol.New(hub)
	control := controlstream.New(nodes, time.Second, hub)
	control.SetMountReporter(broker)
	transport := &mountTransportServer{control: control, acknowledged: make(chan struct{}, 4)}
	tlsConfig, err := authority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	xirangv1.RegisterAgentControlServer(server, transport)
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(listener) }()
	defer func() { server.Stop(); <-served }()
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	panel := NewManagementPanel(directory, state)
	defer panel.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var executions atomic.Int32
	released := make(chan struct{})
	started := make(chan struct{}, 4)
	config := ControlLoopConfig{Address: listener.Addr().String(), StateDirectory: directory, mountExecution: func(ctx context.Context, _ *xirangv1.SharedMountOperation, _ []string) error {
		executions.Add(1)
		started <- struct{}{}
		select {
		case <-released:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	start := func() (context.CancelFunc, <-chan error) {
		sessionCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { _, err := runControlSession(sessionCtx, state, config, executor, nil, nil, panel); done <- err }()
		return stop, done
	}
	waitActive := func() {
		for !hub.NodeActive(state.NodeID) {
			select {
			case <-ctx.Done():
				t.Fatal("Agent did not connect")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	stop, sessionDone := start()
	defer func() { stop(); <-sessionDone }()
	waitActive()
	done := make(chan error, 1)
	go func() { done <- broker.Run(ctx, nodes.node, testMountOperation()) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("operation did not start")
	}
	select {
	case <-transport.acknowledged:
	case <-ctx.Done():
		t.Fatal("no acceptance ACK")
	}
	baseline := nodes.heartbeats.Load()
	for nodes.heartbeats.Load() <= baseline {
		select {
		case err := <-done:
			t.Fatalf("ACK completed unfinished mount: %v", err)
		case <-ctx.Done():
			t.Fatal("heartbeat blocked by helper")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(released)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatal("operation executed more than once")
	}
	// Completion is now lost after the helper succeeds. The API must not claim
	// success; reconnecting cannot automatically execute the old operation again.
	transport.lostResult.Store(true)
	short, shortCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer shortCancel()
	if err := broker.Run(short, nodes.node, testMountOperation()); err == nil {
		t.Fatal("lost result claimed success")
	}
	stop()
	<-sessionDone
	stop, sessionDone = start()
	waitActive()
	heartbeat := nodes.heartbeats.Load()
	for nodes.heartbeats.Load() <= heartbeat {
		select {
		case <-ctx.Done():
			t.Fatal("reconnect heartbeat missing")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if executions.Load() != 2 {
		t.Fatalf("reconnect replayed mutation: %d", executions.Load())
	}
}
