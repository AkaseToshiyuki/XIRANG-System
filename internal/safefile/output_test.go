package safefile

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenRegularRejectsLinksAndPinsOutput(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "secret"), []byte("output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "parent")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"leaf", "parent/secret", "fifo", "bin", "../secret", "/secret"} {
		if file, err := OpenRegular(root, path); err == nil {
			file.Close()
			t.Fatalf("accepted unsafe output %q", path)
		}
	}
	file, err := OpenRegular(root, "bin/secret")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(filepath.Join(root, "bin"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "output" {
		t.Fatalf("descriptor followed replaced parent: %q %v", data, err)
	}
	if file, err := OpenRegular(root, "bin/secret"); err == nil {
		file.Close()
		t.Fatal("reopened replaced parent")
	}
}
