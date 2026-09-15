package backup_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xirang-grid/xirang/internal/backup"
	"github.com/xirang-grid/xirang/internal/domain"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestBackupIsAtomicAndDetectsCorruption(t *testing.T) {
	root := t.TempDir()
	store, err := boltstore.Open(filepath.Join(root, "live", "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.PutQueuePolicy(context.Background(), domain.QueuePolicy{Name: "default", Weight: 1}); err != nil {
		t.Fatal(err)
	}
	service, err := backup.New(filepath.Join(root, "backups"), map[string]backup.SnapshotSource{"xirang.db": store})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := service.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Verify(context.Background(), manifest.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "backups", manifest.ID, "xirang.db")
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("corrupt"), 32); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := service.Verify(context.Background(), manifest.ID); !errors.Is(err, backup.ErrInvalid) {
		t.Fatalf("corrupted backup verified: %v", err)
	}
}

func TestBackupRejectsUnknownManifestFile(t *testing.T) {
	root := t.TempDir()
	store, err := boltstore.Open(filepath.Join(root, "live", "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := backup.New(filepath.Join(root, "backups"), map[string]backup.SnapshotSource{"xirang.db": store})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := service.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manifest.Files[0].Name = "unknown.db"
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "backups", manifest.ID, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Verify(context.Background(), manifest.ID); !errors.Is(err, backup.ErrInvalid) {
		t.Fatalf("unknown manifest file verified: %v", err)
	}
}
