package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/xirang-grid/xirang/internal/backup"
)

type emptyStateSnapshot struct{}

func (emptyStateSnapshot) Snapshot(context.Context, io.Writer) error { return nil }

func TestRelativeStateDirectoryInitializesBackups(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, tc := range []struct{ data, backups, wantData, wantBackups string }{
		{".data/xirang", "", filepath.Join(root, ".data/xirang"), filepath.Join(root, ".data/xirang/backups")},
		{"state/../data", "backup/../custom", filepath.Join(root, "data"), filepath.Join(root, "custom")},
		{filepath.Join(root, "absolute"), filepath.Join(root, "copies"), filepath.Join(root, "absolute"), filepath.Join(root, "copies")},
	} {
		data, backups, err := resolveStateDirectories(tc.data, tc.backups)
		if err != nil || data != tc.wantData || backups != tc.wantBackups {
			t.Fatalf("resolved data=%q backups=%q err=%v", data, backups, err)
		}
		if _, err := backup.New(backups, map[string]backup.SnapshotSource{"state.db": emptyStateSnapshot{}}); err != nil {
			t.Fatalf("resolved CLI paths rejected by backup service: %v", err)
		}
	}
	if _, _, err := resolveStateDirectories("", ""); err == nil {
		t.Fatal("accepted empty state directory")
	}
}
