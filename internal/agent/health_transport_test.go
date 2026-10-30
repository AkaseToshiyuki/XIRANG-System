package agent

import (
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/controlstream"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/pki"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type healthTransportNodes struct{ node domain.Node }

func (nodes healthTransportNodes) Heartbeat(context.Context, string, string) (domain.Node, error) {
	return nodes.node, nil
}
func (nodes healthTransportNodes) GetNode(context.Context, string) (domain.Node, error) {
	return nodes.node, nil
}

type healthTransportServer struct {
	xirangv1.UnimplementedAgentControlServer
	control *controlstream.Server
	loseACK atomic.Bool
}

func (server *healthTransportServer) Connect(stream grpc.BidiStreamingServer[xirangv1.AgentMessage, xirangv1.ControlMessage]) error {
	return server.control.Connect(&healthTransportStream{BidiStreamingServer: stream, server: server})
}

type healthTransportStream struct {
	grpc.BidiStreamingServer[xirangv1.AgentMessage, xirangv1.ControlMessage]
	server *healthTransportServer
}

func (stream *healthTransportStream) Recv() (*xirangv1.AgentMessage, error) {
	message, err := stream.BidiStreamingServer.Recv()
	if err == nil && message.GetHello() != nil {
		if !containsCapability(message.GetHello().Capabilities, hosthealth.OutboxCapability) {
			return nil, status.Error(codes.InvalidArgument, "Agent did not offer durable events")
		}
		// Restrict this transport test to the health path, so no inventory scan,
		// workload or unrelated collector is activated by capability negotiation.
		message.GetHello().Capabilities = []string{"heartbeat-v1", hosthealth.Capability, hosthealth.OutboxCapability}
	}
	return message, err
}
func (stream *healthTransportStream) Send(message *xirangv1.ControlMessage) error {
	if message.GetHostEventsCommitted() != nil && stream.server.loseACK.CompareAndSwap(true, false) {
		return status.Error(codes.Unavailable, "injected disconnect after commit before ACK")
	}
	return stream.BidiStreamingServer.Send(message)
}

func TestHealthOutboxRealMTLSReconnectAfterLostCommitAcknowledgement(t *testing.T) {
	authority, err := pki.OpenOrCreate(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, csr, err := createNodeCSR("health-test")
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := authority.IssueNodeCertificate("node_health", csr)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	state := State{ServerURL: "https://127.0.0.1", NodeID: "node_health", Identity: certificate.Serial}
	if err := saveIdentity(directory, state, key, certificate.CertificatePEM, certificate.CACertificatePEM); err != nil {
		t.Fatal(err)
	}
	repository, err := boltstore.Open(filepath.Join(t.TempDir(), "controller.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	health := hosthealth.New(repository)
	control := controlstream.New(healthTransportNodes{node: domain.Node{ID: state.NodeID, Status: domain.NodeStatusReady, CertificateSerial: certificate.Serial}}, 2*time.Second)
	control.SetHostHealthReporter(health)
	transport := &healthTransportServer{control: control}
	transport.loseACK.Store(true)
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
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); _ = server.Serve(listener) }()
	defer func() { server.Stop(); <-serveDone }()
	outbox, err := openHealthOutbox(directory, state.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.db.Close()
	if _, err := outbox.capture(&xirangv1.HostHealthReport{Events: []*xirangv1.SystemEvent{queueEvent("transport-cursor")}}, journalCheckpoint{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	worker := &healthWorker{outbox: outbox, updates: make(chan struct{}, 1)}
	config := ControlLoopConfig{Address: listener.Addr().String(), StateDirectory: directory, healthWorker: worker}
	executor, err := OpenNativeExecutor(directory)
	if err != nil {
		t.Fatal(err)
	}
	panel := NewManagementPanel(directory, state)
	defer panel.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connected, err := runControlSession(ctx, state, config, executor, nil, nil, panel)
	if !connected || err == nil {
		t.Fatalf("disconnect not observed: %t %v", connected, err)
	}
	if batch, err := outbox.batch(time.Now()); err != nil || batch == nil {
		t.Fatal("lost ACK deleted queued event")
	}
	events, err := health.Events(ctx, state.NodeID, "", 200)
	if err != nil || len(events) != 1 {
		t.Fatal("first session did not commit before disconnect")
	}
	secondCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := runControlSession(secondCtx, state, config, executor, nil, nil, panel); done <- err }()
	defer func() { stop(); <-done }()
	for {
		batch, err := outbox.batch(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if batch == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("reconnected Agent did not consume durable ACK")
		case <-time.After(5 * time.Millisecond):
		}
	}
	events, err = health.Events(ctx, state.NodeID, "", 200)
	if err != nil || len(events) != 1 || events[0].NodeID != state.NodeID {
		t.Fatal("transport replay duplicated or misattributed event")
	}
}
