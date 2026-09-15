package notification

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSecretFileRequiresPrivateRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhook.secret")
	if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := LoadSecretFile(path)
	if err != nil || string(secret) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("secret=%q err=%v", secret, err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecretFile(path); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("group-readable secret error = %v", err)
	}
}
