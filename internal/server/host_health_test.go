package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/controlstream"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type healthTestNodes struct{}

func (healthTestNodes) GetNode(_ context.Context, id string) (domain.Node, error) {
	return domain.Node{ID: id, Status: domain.NodeStatusReady, Capabilities: []string{hosthealth.Capability, hosthealth.IdentifyCapability}}, nil
}

func TestHostHealthRoutesAreAdminOnlyAndLocatorHasAcknowledgement(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := hosthealth.New(store)
	access := identity.NewAccessService(store, nil, nil)
	viewer, err := access.CreateToken(context.Background(), "read", "viewer", time.Hour, domain.Principal{Role: "admin", Subject: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	commands := controlstream.NewHub(4)
	registerHostHealthRoutes(mux, service, healthTestNodes{}, commands, "admin")
	handler := accessMiddleware(access, "admin", mux)
	report := &xirangv1.HostHealthReport{ObservedAt: timestamppb.Now(), IdentifySupported: true}
	if err := service.Report(context.Background(), "node", report); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/nodes/node/health", "/api/v1/nodes/node/health/events"} {
		for _, auth := range []struct {
			token  string
			status int
		}{{"", 401}, {viewer.Secret, 403}, {"admin", 200}} {
			response := jsonRequest(t, handler, "GET", path, nil, auth.token)
			if response.Code != auth.status {
				t.Fatalf("%s: status %d want %d", path, response.Code, auth.status)
			}
		}
	}
	for _, action := range []string{"power-off", "sel-clear", "raw"} {
		response := jsonRequest(t, handler, "POST", "/api/v1/nodes/node/health/actions", map[string]string{"action": action}, "admin")
		if response.Code != 400 {
			t.Fatalf("unsupported operation accepted: %s %d", action, response.Code)
		}
	}
	response := jsonRequest(t, handler, "POST", "/api/v1/nodes/node/health/actions", map[string]string{"action": "identify-on"}, "admin")
	if response.Code != 202 {
		t.Fatalf("locator: %d %s", response.Code, response.Body.String())
	}
	var action domain.BMCAction
	if err := json.Unmarshal(response.Body.Bytes(), &action); err != nil {
		t.Fatal(err)
	}
	if action.Status != "queued" {
		t.Fatal("queued command was reported executed")
	}
	session := commands.Attach("node")
	defer session.Detach()
	queued, ok := session.Take()
	if !ok || queued.MessageId != action.ID || queued.GetSetBmcIdentify() == nil || !queued.GetSetBmcIdentify().Enabled {
		t.Fatalf("locator did not reach the real control queue: %v", queued)
	}
	if err := service.ConfirmAction(context.Background(), "different-node", action.ID, true, "ok"); err == nil {
		t.Fatal("wrong node confirmed command")
	}
	if err := service.ConfirmAction(context.Background(), "node", action.ID, true, "acknowledged"); err != nil {
		t.Fatal(err)
	}
	response = jsonRequest(t, handler, "GET", "/api/v1/nodes/node/health/actions/"+action.ID, nil, "admin")
	if response.Code != 200 {
		t.Fatalf("action read: %d", response.Code)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &action); err != nil || action.Status != "succeeded" || action.CompletedAt == nil {
		t.Fatalf("action: %+v %v", action, err)
	}
	response = jsonRequest(t, handler, "GET", "/api/v1/nodes/other/health/actions/"+action.ID, nil, "admin")
	if response.Code != 404 {
		t.Fatal("action leaked across node path")
	}
	report.ObservedAt = timestamppb.New(time.Now().Add(time.Second))
	report.IdentifySupported = false
	if err := service.Report(context.Background(), "node", report); err != nil {
		t.Fatal(err)
	}
	response = jsonRequest(t, handler, "POST", "/api/v1/nodes/node/health/actions", map[string]string{"action": "identify-on"}, "admin")
	if response.Code != 503 {
		t.Fatal("host-disabled locator request was queued")
	}
}
