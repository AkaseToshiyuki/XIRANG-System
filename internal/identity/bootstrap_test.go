package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateAdminToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.token")
	first, created, err := LoadOrCreateAdminToken(path)
	if err != nil || !created || first == "" {
		t.Fatalf("create token: token=%q created=%v err=%v", first, created, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected token mode: %o", info.Mode().Perm())
	}
	second, created, err := LoadOrCreateAdminToken(path)
	if err != nil || created || second != first {
		t.Fatalf("reload token: token=%q created=%v err=%v", second, created, err)
	}
	if !AuthenticateBearer("Bearer "+first, first) || AuthenticateBearer("Bearer wrong", first) {
		t.Fatal("unexpected bearer authentication result")
	}
}
