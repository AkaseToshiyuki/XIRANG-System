package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"golang.org/x/sys/unix"
)

func TestLocalArtifactRejectsSpecialFilesWithoutBlockingStaging(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "fifo-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	executor := &NativeExecutor{workspaceDirectory: filepath.Join(root, "work"), cacheDirectory: filepath.Join(root, "cache")}
	if err := os.MkdirAll(executor.cacheDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link, root, "/dev/null"} {
		done := make(chan error, 1)
		go func() {
			_, _, err := executor.stageArtifact(context.Background(), &xirangv1.RunAssignment{AssignmentId: "special", Attempt: 1, Artifact: &xirangv1.NativeArtifact{Uri: "file://" + path, Sha256: strings.Repeat("0", 64), Entrypoint: "program"}})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("accepted non-regular artifact %s", path)
			}
		case <-time.After(time.Second):
			// Unblock a regressed FIFO open so a failing test leaves no goroutine
			// holding the process-global cache mutex for subsequent tests.
			fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
			if err == nil {
				_ = unix.Close(fd)
			}
			t.Fatal("special artifact blocked staging")
		}
	}
	payload := []byte("legitimate executable")
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	regularLink := filepath.Join(root, "regular-link")
	if err := os.Symlink(regular, regularLink); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"file://" + regular, "file://localhost" + regular, "file://" + regularLink} {
		// Force a miss for every URI, including localhost and regular symlinks.
		_ = os.Remove(filepath.Join(executor.cacheDirectory, hex.EncodeToString(digest[:])))
		_, output, err := executor.stageArtifact(context.Background(), &xirangv1.RunAssignment{AssignmentId: "normal", Attempt: 1, Artifact: &xirangv1.NativeArtifact{Uri: uri, Sha256: hex.EncodeToString(digest[:]), Entrypoint: "program"}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(output)
		if err != nil || string(got) != string(payload) {
			t.Fatalf("normal staging: %q %v", got, err)
		}
	}
	if err := ensureCachedArtifact(context.Background(), "file://"+regular, strings.Repeat("0", 64), filepath.Join(root, "bad-hash")); err == nil {
		t.Fatal("bad digest accepted")
	}
}

func TestLocalArtifactCanceledBeforeOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := openArtifact(ctx, "file:///unused"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open: %v", err)
	}
}
