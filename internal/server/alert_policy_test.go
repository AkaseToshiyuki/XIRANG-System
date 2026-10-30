package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/alertpolicy"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestAlertPolicyAuthorizationConflictAuditAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	access := identity.NewAccessService(store, nil, nil)
	actor := domain.Principal{Subject: "admin", Role: "admin", Permissions: []string{"*"}, AuthMethod: "api_token"}
	viewer, err := access.CreateToken(ctx, "viewer", "viewer", time.Hour, actor)
	if err != nil {
		t.Fatal(err)
	}
	defaults := alertpolicy.Defaults(7*time.Second, 11*time.Second)
	service := alertpolicy.New(store, defaults)
	handler := New(Config{AdminToken: "admin", Access: access, AlertPolicy: service}).HTTPHandler()
	for _, check := range []struct {
		method, token string
		want          int
	}{
		{"GET", "", 401}, {"GET", viewer.Secret, 200}, {"PUT", viewer.Secret, 403}, {"PUT", "wrong", 401},
	} {
		response := jsonRequest(t, handler, check.method, "/api/v1/alert-policy", defaults, check.token)
		if response.Code != check.want {
			t.Fatalf("%s: %d %s", check.method, response.Code, response.Body.String())
		}
	}
	initial, err := service.Get(ctx)
	if err != nil || initial.TriggerAfterSeconds != 7 || initial.ResolveAfterSeconds != 11 || initial.Revision != 0 {
		t.Fatalf("configured defaults lost: %+v %v", initial, err)
	}
	input := initial
	input.TriggerAfterSeconds, input.MinimumSeverity, input.RouteEnabled = 13, domain.AlertSeverityCritical, false
	response := jsonRequest(t, handler, http.MethodPut, "/api/v1/alert-policy", input, "admin")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var saved domain.AlertPolicy
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 {
		t.Fatal("missing revision")
	}
	if response := jsonRequest(t, handler, http.MethodPut, "/api/v1/alert-policy", input, "admin"); response.Code != 409 {
		t.Fatal("stale edit overwrote policy")
	}
	bad := saved
	bad.Kinds = []string{"interface_health", "interface_health"}
	if response := jsonRequest(t, handler, http.MethodPut, "/api/v1/alert-policy", bad, "admin"); response.Code != 400 {
		t.Fatal("duplicate routes accepted")
	}
	bad = saved
	bad.TriggerAfterSeconds = -1
	if response := jsonRequest(t, handler, http.MethodPut, "/api/v1/alert-policy", bad, "admin"); response.Code != 400 {
		t.Fatal("negative duration accepted")
	}
	bad = saved
	far := time.Now().Add(31 * 24 * time.Hour)
	bad.SilenceUntil = &far
	if response := jsonRequest(t, handler, http.MethodPut, "/api/v1/alert-policy", bad, "admin"); response.Code != 400 {
		t.Fatal("unbounded silence accepted")
	}
	audits, err := store.ListAuditEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, audit := range audits {
		if audit.Action == "alerts.policy.update" {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("successful policy changes audited %d times", updates)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := alertpolicy.New(store, alertpolicy.Defaults(time.Second, time.Second)).Get(ctx)
	if err != nil || current.Revision != 1 || current.TriggerAfterSeconds != 13 || current.RouteEnabled || current.MinimumSeverity != domain.AlertSeverityCritical {
		t.Fatalf("restart lost persisted policy: %+v %v", current, err)
	}
}
