package agent

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOwnedDeleteRejectsDirectoryReplacedAfterValidation(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "owned", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{filepath.Join(root, "owned", "child", "item"), filepath.Join(outside, "item")} {
		if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	entry, err := inspectOwnedDelete(fd, "owned", "owned", map[string]struct{}{"owned": {}, "owned/child": {}, "owned/child/item": {}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "owned", "child"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "owned", "child")); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedEntry(fd, entry); err == nil {
		t.Fatal("accepted replacement directory link")
	}
	data, err := os.ReadFile(filepath.Join(outside, "item"))
	if err != nil || string(data) != "keep" {
		t.Fatalf("deleted outside file: %q %v", data, err)
	}
}

func TestOwnedDeletePreservesLateUntrackedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "owned"), 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	entry, err := inspectOwnedDelete(fd, "owned", "owned", map[string]struct{}{"owned": {}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "owned", "late")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedEntry(fd, entry); err == nil {
		t.Fatal("removed directory containing late arrival")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
}
