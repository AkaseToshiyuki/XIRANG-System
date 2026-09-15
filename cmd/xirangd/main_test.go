package main

import (
	"context"
	"net"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// stubAgentControl echoes one control message so a stream can be exercised
// without the full control-plane wiring.
type stubAgentControl struct {
	xirangv1.UnimplementedAgentControlServer
}

func (stubAgentControl) Connect(stream grpc.BidiStreamingServer[xirangv1.AgentMessage, xirangv1.ControlMessage]) error {
	for {
		message, err := stream.Recv()
		if err != nil {
			return nil
		}
		if err := stream.Send(&xirangv1.ControlMessage{MessageId: message.MessageId}); err != nil {
			return err
		}
	}
}

// TestAgentTransportLimitsAdmitAnAgentStream checks the transport limits
// against a client dialled the way the Agent dials. The limits exist to bound
// what a misbehaving peer can do, and a keepalive policy stricter than the
// Agent's own behaviour would disconnect every legitimate node instead.
func TestAgentTransportLimitsAdmitAnAgentStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(agentServerOptions()...)
	xirangv1.RegisterAgentControlServer(server, stubAgentControl{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// The Agent sets call sizes and transport credentials but no keepalive of
	// its own, which is what makes the enforcement policy above safe.
	connection, err := grpc.NewClient(listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20), grpc.MaxCallSendMsgSize(1<<20)))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	stream, err := xirangv1.NewAgentControlClient(connection).Connect(ctx)
	if err != nil {
		t.Fatalf("the transport limits refused an Agent-style stream: %v", err)
	}
	if err := stream.Send(&xirangv1.AgentMessage{MessageId: "probe"}); err != nil {
		t.Fatalf("sending on the stream failed: %v", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		t.Fatalf("receiving on the stream failed: %v", err)
	}
	if reply.MessageId != "probe" {
		t.Fatalf("unexpected reply over the control stream: %q", reply.MessageId)
	}

	// Hold the stream open past a few keepalive intervals' worth of idle time.
	// The client answers server pings at the transport layer, so an idle
	// control stream must survive rather than being reclaimed underneath a node
	// that simply has nothing to report.
	time.Sleep(2 * time.Second)
	if err := stream.Send(&xirangv1.AgentMessage{MessageId: "after-idle"}); err != nil {
		t.Fatalf("the stream did not survive an idle period: %v", err)
	}
	if reply, err = stream.Recv(); err != nil || reply.MessageId != "after-idle" {
		t.Fatalf("the stream stopped working after idling: reply=%v err=%v", reply, err)
	}
}
