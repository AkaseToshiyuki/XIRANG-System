//go:build linux

package build

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestKillBuildProcessGroupTerminatesDescendants(t *testing.T) {
	command := exec.Command("sh", "-c", "sleep 30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := killBuildProcessTree(command, filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("process group unexpectedly exited successfully")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("process-group cancellation took %s", elapsed)
	}
}
