//go:build linux

package build

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReclaimOutputRejectsLeafAndParentLinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "parent")); err != nil {
		t.Fatal(err)
	}
	for _, shared := range []bool{false, true} {
		runner := &NativeRunner{shared: shared}
		for _, relative := range []string{"leaf", "parent/secret"} {
			if err := runner.reclaimOutput(root, relative); err == nil {
				t.Fatalf("reclaimed linked output: %s", relative)
			}
		}
	}
	info, err := os.Stat(secret)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("changed external secret mode: %v %v", info, err)
	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "sensitive" {
		t.Fatalf("changed external contents: %q %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(root, "regular"), []byte("output"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (&NativeRunner{shared: true}).reclaimOutput(root, "regular"); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(root, "regular"))
	if err != nil || info.Mode().Perm() != 0o550 {
		t.Fatalf("regular output not protected: %v %v", info, err)
	}
}

func TestStopBuildCgroupRequiresObservedEmpty(t *testing.T) {
	root := t.TempDir()
	if err := stopBuildCgroup(root); err == nil {
		t.Fatal("accepted missing cgroup events")
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.events"), []byte("populated 0\nfrozen 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stopBuildCgroup(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.kill")); !os.IsNotExist(err) {
		t.Fatalf("an already empty cgroup should not require cgroup.kill: %v", err)
	}
}

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
