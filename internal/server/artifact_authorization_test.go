package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

type artifactBuildManager struct {
	BuildManager
	build domain.Build
}

func (manager *artifactBuildManager) Get(_ context.Context, id string) (domain.Build, error) {
	if id != manager.build.ID {
		return domain.Build{}, errors.New("build not found")
	}
	return manager.build, nil
}
func (*artifactBuildManager) Workers() []domain.BuildWorker { return nil }

func TestArtifactDownloadsEnforceBuildOwnership(t *testing.T) {
	root := t.TempDir()
	store, err := boltstore.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	manager := &artifactBuildManager{build: domain.Build{ID: "build_owned", OwnerID: "alice"}}
	for _, name := range []string{"program", "program.spdx.json"} {
		if err := os.MkdirAll(filepath.Join(root, "build_owned"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "build_owned", name), []byte("private artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(Config{ArtifactRoot: root, Access: access, Builds: manager, AdminToken: "bootstrap"}).HTTPHandler()
	for _, tc := range []struct {
		subject, role string
		want          int
	}{{"alice", "builder", 200}, {"bob", "builder", 404}, {"bob", "user", 404}, {"bob", "viewer", 404}, {"bob", "admin", 200}} {
		token, err := access.CreateToken(context.Background(), "download", tc.role, time.Hour, domain.Principal{Subject: tc.subject})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"program", "program.spdx.json"} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				response := jsonRequest(t, handler, method, "/artifacts/build_owned/"+name, nil, token.Secret)
				if response.Code != tc.want {
					t.Fatalf("subject=%s role=%s method=%s artifact=%s status=%d", tc.subject, tc.role, method, name, response.Code)
				}
				if tc.want == 200 && response.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("private artifact may be cached publicly")
				}
			}
		}
	}
	// A deployment without build metadata must not grant human users blanket
	// read access merely because their token authenticates successfully.
	viewer, err := access.CreateToken(context.Background(), "viewer", "viewer", time.Hour, domain.Principal{Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "build_other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build_other", "secret"), []byte("other owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "build_other", "secret"), filepath.Join(root, "build_owned", "alias")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/artifacts/build_other/secret", "/artifacts/missing/program", "/artifacts/build_owned/alias"} {
		if got := jsonRequest(t, handler, http.MethodGet, path, nil, viewer.Secret); got.Code != 404 {
			t.Fatalf("cross-owner path %s: %d", path, got.Code)
		}
	}
	withoutBuilds := New(Config{ArtifactRoot: root, Access: access}).HTTPHandler()
	if got := jsonRequest(t, withoutBuilds, http.MethodGet, "/artifacts/build_owned/program", nil, viewer.Secret); got.Code != 404 {
		t.Fatalf("missing metadata allowed artifact: %d", got.Code)
	}
}
