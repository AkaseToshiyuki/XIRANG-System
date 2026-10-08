//go:build linux

package build

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func leaseTestRunner(root string) *NativeRunner {
	return &NativeRunner{lockRoot: root, credential: &syscall.Credential{Uid: 400000001, Gid: 400000001}}
}

func TestBuildIdentityLeaseProcess(t *testing.T) {
	if root := os.Getenv("XIRANG_LEASE_TEST_ROOT"); root != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		release, err := leaseTestRunner(root).AcquireBuild(ctx)
		if release != nil {
			release()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cross-process lease: %v", err)
		}
		return
	}
	root := t.TempDir()
	first := leaseTestRunner(root)
	release, err := first.AcquireBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildIdentityLeaseProcess$")
	cmd.Env = append(os.Environ(), "XIRANG_LEASE_TEST_ROOT="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	second := leaseTestRunner(root)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := second.AcquireBuild(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-process wait: %v", err)
	}
	release()
	next, err := second.AcquireBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestBuildIdentityLeaseQuarantine(t *testing.T) {
	root := t.TempDir()
	runner := leaseTestRunner(root)
	release, err := runner.AcquireBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An invalid workspace root cannot be reclaimed, and must poison the UID.
	outside := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := runner.CleanupWorkspace(link); err == nil {
		t.Fatal("symlink workspace accepted")
	}
	release()
	if next, err := leaseTestRunner(root).AcquireBuild(context.Background()); err == nil {
		next()
		t.Fatal("quarantine did not survive a new runner")
	}
}

func TestBuildIdentityLeaseRejectsWritableNamespace(t *testing.T) {
	root := t.TempDir()
	runner := leaseTestRunner(root)
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if release, err := runner.AcquireBuild(context.Background()); err == nil {
		release()
		t.Fatal("world-writable namespace accepted")
	}
}

func TestBuildWorkspaceCleanupPreservesSymlinkTarget(t *testing.T) {
	runner := leaseTestRunner(t.TempDir())
	root := t.TempDir()
	workspace := filepath.Join(root, "work")
	nested := filepath.Join(workspace, "a", "b")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(nested, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0000); err != nil {
		t.Fatal(err)
	}
	if err := runner.CleanupWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remains: %v", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "private" {
		t.Fatalf("outside changed: %q %v", data, err)
	}
}

func TestBuildIdentityLeaseCrashRecovery(t *testing.T) {
	if root := os.Getenv("XIRANG_LEASE_CRASH_ROOT"); root != "" {
		if _, err := leaseTestRunner(root).AcquireBuild(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Process exit releases flock without proving that processes/files are gone.
		os.Exit(0)
	}
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildIdentityLeaseCrashRecovery$")
	cmd.Env = append(os.Environ(), "XIRANG_LEASE_CRASH_ROOT="+root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v %s", err, output)
	}
	if release, err := leaseTestRunner(root).AcquireBuild(context.Background()); err == nil {
		release()
		t.Fatal("crashed lease was reused")
	}
}
