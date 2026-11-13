package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

// Multi-request RBAC fixtures use the same scoped session exchange as the UI.
// The raw API credential's one-use behavior is covered separately below.
func createTestSession(t *testing.T, access *identity.AccessService, ctx context.Context, name, role string, ttl time.Duration, actor domain.Principal) (identity.CreatedAPIToken, error) {
	t.Helper()
	created, err := access.CreateToken(ctx, name, role, ttl, actor)
	if err != nil {
		return identity.CreatedAPIToken{}, err
	}
	return access.ExchangeToken(ctx, created.Secret)
}

func TestOneUseTokenHTTPAuthenticationAndConsoleExchange(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	handler := New(Config{AdminToken: "bootstrap", Access: access}).HTTPHandler()
	create := func() identity.CreatedAPIToken {
		t.Helper()
		token, err := access.CreateToken(context.Background(), "once", "viewer", time.Hour, domain.Principal{Subject: "test-user"})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	raw := create()
	first := jsonRequest(t, handler, http.MethodGet, "/api/v1/access/me", nil, raw.Secret)
	if first.Code != http.StatusOK {
		t.Fatalf("first authentication: %d %s", first.Code, first.Body.String())
	}
	reused := jsonRequest(t, handler, http.MethodGet, "/api/v1/access/me", nil, raw.Secret)
	if reused.Code != http.StatusUnauthorized {
		t.Fatalf("token reused: %d", reused.Code)
	}
	other := create()
	exchanged := jsonRequest(t, handler, http.MethodPost, "/api/v1/auth/token", map[string]string{"token": other.Secret}, "")
	if exchanged.Code != http.StatusOK || exchanged.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange: %d %s", exchanged.Code, exchanged.Body.String())
	}
	var session identity.CreatedAPIToken
	if err := json.Unmarshal(exchanged.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		response := jsonRequest(t, handler, http.MethodGet, "/api/v1/access/me", nil, session.Secret)
		if response.Code != http.StatusOK {
			t.Fatalf("session request %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	for _, secret := range []string{other.Secret, session.Secret} {
		response := jsonRequest(t, handler, http.MethodPost, "/api/v1/auth/token", map[string]string{"token": secret}, "")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("consumed token/session exchanged: %d %s", response.Code, response.Body.String())
		}
	}
	forbidden := jsonRequest(t, handler, http.MethodPost, "/api/v1/access/tokens", map[string]any{"name": "escalated", "role": "admin", "ttlSeconds": 3600}, session.Secret)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("viewer session privilege escalation: %d", forbidden.Code)
	}
}

func TestAlertIgnoreHTTPPermissionAndOperatorIdentity(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	manager := &testAlertManager{}
	handler := New(Config{AdminToken: "bootstrap", Access: access, Alerts: manager}).HTTPHandler()
	for _, role := range []string{"viewer", "admin"} {
		created, err := access.CreateToken(context.Background(), "ignore test", role, time.Hour, domain.Principal{Subject: "operator-alice"})
		if err != nil {
			t.Fatal(err)
		}
		response := jsonRequest(t, handler, http.MethodPost, "/api/v1/alerts/alert-1/ignore", map[string]uint64{"revision": 1}, created.Secret)
		if role == "viewer" {
			if response.Code != http.StatusForbidden || manager.acknowledgedID != "" {
				t.Fatalf("viewer ignored alert: %d", response.Code)
			}
		} else if response.Code != http.StatusOK || manager.actor != "operator-alice" || manager.acknowledgedID != "alert-1" {
			t.Fatalf("ignore lost operator identity: %d %s actor=%s", response.Code, response.Body.String(), manager.actor)
		}
	}
}
