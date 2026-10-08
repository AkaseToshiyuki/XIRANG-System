package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestFinalizeArtifactRejectsLinkedOutput(t *testing.T) {
	workspace, outside, artifacts := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(workspace, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "parent")); err != nil {
		t.Fatal(err)
	}
	service := &Service{artifactRoot: artifacts}
	for _, path := range []string{"leaf", "parent/secret"} {
		if _, err := service.finalizeArtifact(domain.Build{ID: "build_link", Recipe: domain.BuildRecipe{OutputPath: path}}, workspace); err == nil {
			t.Fatalf("finalized linked output: %s", path)
		}
	}
	entries, err := os.ReadDir(artifacts)
	if err != nil || len(entries) != 0 {
		t.Fatalf("published private bytes: %v %v", entries, err)
	}
}
