package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/networkhealth"
	"github.com/xirang-grid/xirang/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

func TestControlTransportUsesCachedAlternateWithCanonicalTLSIdentity(t *testing.T) {
	authority, err := pki.OpenOrCreate(t.TempDir(), []string{"controller.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	serverTrust, err := authority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	serverTrust.ClientAuth = tls.RequireAndVerifyClientCert
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTrust)))
	healthv1.RegisterHealthServer(server, health.NewServer())
	go server.Serve(listener)
	defer server.Stop()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	_, key, csr, err := createNodeCSR("control-path-test")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueNodeCertificate("node-control-path", csr)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	state := State{ServerURL: "https://controller.invalid:8899", NodeID: "node-control-path", Identity: issued.Serial}
	if err := saveIdentity(directory, state, key, issued.CertificatePEM, issued.CACertificatePEM); err != nil {
		t.Fatal(err)
	}
	// Expired observations are recovery candidates only: this test requires a
	// new authenticated connection, and never asserts fresh ranking evidence.
	path := networkhealth.Path{SourceAddress: "127.0.0.1", SourceInterface: "lo", TargetAddress: "127.0.0.1", ExpiresAt: time.Now().Add(-time.Hour)}
	encoded, _ := json.Marshal(controllerPathCache{ServerURL: state.ServerURL, NodeID: state.NodeID, Paths: []networkhealth.Path{path}})
	if err := atomicWrite(filepath.Join(directory, "network-paths.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	trust, err := authenticatedTLSConfig(directory, state)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, selected, err := dialSelectedControl(ctx, state, ControlLoopConfig{Address: "controller.invalid:" + port, StateDirectory: directory}, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if selected != controllerPathKey(path) {
		t.Fatalf("selected %q", selected)
	}
	result, err := healthv1.NewHealthClient(connection).Check(ctx, &healthv1.HealthCheckRequest{})
	if err != nil || result.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		t.Fatalf("actual RPC failed: %v %v", result, err)
	}
}

func TestControllerHTTPRoutesPreserveMTLSIdentityAndVerifyFallback(t *testing.T) {
	if _, err := net.InterfaceByName("lo"); err != nil {
		t.Skip("requires Linux loopback")
	}
	authority, err := pki.OpenOrCreate(t.TempDir(), []string{"controller.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	serverTrust, err := authority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	serverTrust.ClientAuth = tls.RequireAndVerifyClientCert
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "controller.invalid" || len(r.TLS.VerifiedChains) == 0 {
			t.Error("transport lost the original TLS authority or client identity")
		}
		requests.Add(1)
		w.Write([]byte("selected-path"))
	}))
	server.TLS = serverTrust
	server.StartTLS()
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	_, key, csr, err := createNodeCSR("path-test")
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueNodeCertificate("node-path", csr)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	state := State{ServerURL: "https://controller.invalid:" + port, NodeID: "node-path", Identity: issued.Serial}
	if err := saveIdentity(directory, state, key, issued.CertificatePEM, issued.CACertificatePEM); err != nil {
		t.Fatal(err)
	}
	// The higher-ranked address has a TLS listener with a different CA. It must
	// never receive an authenticated application request or prevent fallback.
	otherAuthority, err := pki.OpenOrCreate(t.TempDir(), []string{"controller.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	otherTrust, err := otherAuthority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	otherListener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Fatal(err)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted endpoint received request") }))
	other.Listener.Close()
	other.Listener = otherListener
	other.TLS = otherTrust
	other.StartTLS()
	defer other.Close()
	cache := controllerPathCache{ServerURL: state.ServerURL, NodeID: state.NodeID, Paths: []networkhealth.Path{
		{SourceAddress: "127.0.0.1", SourceInterface: "lo", TargetAddress: "127.0.0.2", ExpiresAt: time.Now().Add(time.Minute)},
		{SourceAddress: "127.0.0.1", SourceInterface: "lo", TargetAddress: "127.0.0.1", ExpiresAt: time.Now().Add(time.Minute)},
	}}
	encoded, _ := json.Marshal(cache)
	if err := atomicWrite(filepath.Join(directory, "network-paths.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := authenticatedHTTPClient(directory, state)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(state.ServerURL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "selected-path" || requests.Load() != 1 {
		t.Fatalf("authenticated fallback failed: %s %v", body, err)
	}
	if _, err := client.Get("https://127.0.0.1:" + port + "/probe"); err == nil {
		t.Fatal("authenticated transport allowed a different origin")
	}
	if paths := loadControllerPaths(directory, State{NodeID: "other", ServerURL: state.ServerURL}); len(paths) != 0 {
		t.Fatal("cache crossed node identity")
	}
}
