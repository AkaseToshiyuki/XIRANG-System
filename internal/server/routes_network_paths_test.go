package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/networkhealth"
	"github.com/xirang-grid/xirang/internal/pki"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestNetworkPathEndpointRequiresActiveSelfCertificate(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := enrollment.NewService(store, authority)
	token, err := service.CreateToken(context.Background(), enrollment.TokenOptions{Name: "paths", TTL: time.Hour, MaxUses: 1})
	if err != nil {
		t.Fatal(err)
	}
	registered, err := service.Register(context.Background(), token.Secret, serverTestCSR(t), domain.InventorySummary{Hostname: "host", OS: "linux", Architecture: "amd64", CPUCount: 1, MemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(registered.CertificatePEM))
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &networkhealth.Resolver{LocalID: registered.Node.ID, Nodes: service}
	handler := New(Config{AdminToken: "admin", Enrollment: service, NetworkPaths: resolver}).HTTPHandler()
	request := func(query string, authenticated bool) int {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/network-paths"+query, nil)
		if authenticated {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("", false); got != http.StatusUnauthorized {
		t.Fatalf("anonymous %d", got)
	}
	if got := request("", true); got != http.StatusOK {
		t.Fatalf("active certificate %d", got)
	}
	if got := request("?source=someone-else", true); got != http.StatusUnauthorized {
		t.Fatalf("other source %d", got)
	}
	if _, err := service.RevokeCertificate(context.Background(), registered.Node.ID); err != nil {
		t.Fatal(err)
	}
	if got := request("", true); got != http.StatusUnauthorized {
		t.Fatalf("revoked certificate %d", got)
	}
}
