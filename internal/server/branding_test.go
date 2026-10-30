package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/branding"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestClusterBrandingAuthorizationValidationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	access := identity.NewAccessService(store, nil, nil)
	actor := domain.Principal{Subject: "admin", Role: "admin", Permissions: []string{"*"}, AuthMethod: "api_token"}
	viewer, err := access.CreateToken(context.Background(), "viewer", "viewer", time.Hour, actor)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{AdminToken: "admin", Access: access, Branding: branding.New(store)}).HTTPHandler()
	get := func() domain.ClusterBranding {
		t.Helper()
		response := jsonRequest(t, handler, http.MethodGet, "/api/v1/cluster/branding", nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("get: %d %s", response.Code, response.Body.String())
		}
		var value domain.ClusterBranding
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if value := get(); value.Name != "XIRANG" || value.Revision != 0 || value.LogoDataURL != "" {
		t.Fatalf("default: %+v", value)
	}
	input := domain.ClusterBranding{Name: " 科学计算集群 ", Revision: 0}
	for _, denied := range []struct {
		token  string
		status int
	}{{"", 401}, {"wrong", 401}, {viewer.Secret, 403}} {
		response := jsonRequest(t, handler, http.MethodPut, "/api/v1/cluster/branding", input, denied.token)
		if response.Code != denied.status {
			t.Fatalf("authorization: want %d got %d %s", denied.status, response.Code, response.Body.String())
		}
	}
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	input.LogoDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(pngData.Bytes(), []byte("untrusted trailing data")...))
	response := jsonRequest(t, handler, http.MethodPut, "/api/v1/cluster/branding", input, "admin")
	if response.Code != http.StatusOK {
		t.Fatalf("save: %d %s", response.Code, response.Body.String())
	}
	saved := get()
	if saved.Name != "科学计算集群" || saved.Revision != 1 || saved.LogoDataURL == input.LogoDataURL {
		t.Fatalf("canonical value: %+v", saved)
	}
	if response := jsonRequest(t, handler, http.MethodPut, "/api/v1/cluster/branding", input, "admin"); response.Code != 409 {
		t.Fatalf("stale update: %d", response.Code)
	}
	for _, invalid := range []domain.ClusterBranding{
		{Name: "", Revision: 1}, {Name: strings.Repeat("界", 65), Revision: 1}, {Name: "bad\nname", Revision: 1},
		{Name: "valid", LogoDataURL: "https://remote.example/logo.png", Revision: 1},
		{Name: "valid", LogoDataURL: "data:image/svg+xml;base64,PHN2Zy8+", Revision: 1},
		{Name: "valid", LogoDataURL: "data:image/png;base64,bm90LWFuLWltYWdl", Revision: 1},
		{Name: "valid", LogoDataURL: "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(branding.MaxLogoBytes)+4), Revision: 1},
	} {
		response := jsonRequest(t, handler, http.MethodPut, "/api/v1/cluster/branding", invalid, "admin")
		if response.Code != 400 {
			t.Fatalf("invalid accepted: status=%d", response.Code)
		}
		if current := get(); current.Revision != saved.Revision {
			t.Fatal("invalid upload changed stored settings")
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler = New(Config{AdminToken: "admin", Branding: branding.New(store)}).HTTPHandler()
	if current := get(); current.Name != saved.Name || current.LogoDataURL != saved.LogoDataURL || current.Revision != 1 {
		t.Fatalf("restart lost settings: %+v", current)
	}
	response = jsonRequest(t, handler, http.MethodPut, "/api/v1/cluster/branding", domain.ClusterBranding{Name: "XIRANG", Revision: 1}, "admin")
	if response.Code != 200 || get().LogoDataURL != "" {
		t.Fatalf("reset: %d %s", response.Code, response.Body.String())
	}
}
