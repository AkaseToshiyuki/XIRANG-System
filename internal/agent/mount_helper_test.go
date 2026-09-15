package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountHelperInputValidation(t *testing.T) {
	if !safeMountID("share_123") || safeMountID("../bad") {
		t.Fatal("mount ID validation failed")
	}
	if !safeMountHost("192.0.2.25") || safeMountHost("host;reboot") {
		t.Fatal("mount host validation failed")
	}
	if !safeMountPath("/srv/xirang/project-1") || safeMountPath("/") || safeMountPath("/tmp/a b") {
		t.Fatal("mount path validation failed")
	}
	if !safeSharedPolicyPath("/shared/project-1") || safeSharedPolicyPath("/etc/xirang") || safeSharedPolicyPath("/home/user") {
		t.Fatal("shared path policy root validation failed")
	}
}

func TestSystemdSharedPathPolicyUsesExactSortedPaths(t *testing.T) {
	root := t.TempDir()
	policies := filepath.Join(root, "policies")
	systemd := filepath.Join(root, "systemd")
	if err := writeSystemdSharedPathPolicy(policies, systemd, "space-b", "/shared/b", true); err != nil {
		t.Fatal(err)
	}
	if err := writeSystemdSharedPathPolicy(policies, systemd, "space-a", "/srv/a", true); err != nil {
		t.Fatal(err)
	}
	for _, service := range sharedPathPolicyServices {
		content, err := os.ReadFile(filepath.Join(systemd, service+".d", "30-xirang-shared-paths.conf"))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "[Service]\nReadWritePaths=/shared/b /srv/a\n" {
			t.Fatalf("unexpected %s policy: %q", service, content)
		}
	}
	if err := writeSystemdSharedPathPolicy(policies, systemd, "space-b", "/shared/b", false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(systemd, "xirangd.service.d", "30-xirang-shared-paths.conf"))
	if err != nil || string(content) != "[Service]\nReadWritePaths=/srv/a\n" {
		t.Fatalf("unexpected revoked policy: %q err=%v", content, err)
	}
}

func TestRemoveOwnedTreeDeletesOnlyDeclaredEntries(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "alice")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "input.dat"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal([]string{"alice", "alice/input.dat"})
	if err := removeOwnedTree(directory, "alice", base64.RawURLEncoding.EncodeToString(manifest)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("owned directory still exists: %v", err)
	}
}

func TestRemoveOwnedTreeRefusesUntrackedDescendant(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "alice")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "unexpected.dat")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal([]string{"alice"})
	if err := removeOwnedTree(directory, "alice", base64.RawURLEncoding.EncodeToString(manifest)); err == nil {
		t.Fatal("untracked descendant was deleted")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("untracked file was not preserved: %v", err)
	}
}

func TestDescriptorRelativeSharedPathRejectsSymlinkedParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if fd, _, err := openSharedParent(root, "escape/result.dat", false); err == nil {
		unix.Close(fd)
		t.Fatal("symlinked shared parent was accepted")
	}
}

func TestDescriptorRelativeSharedPathPinsValidatedParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	original := filepath.Join(root, "owned")
	moved := filepath.Join(root, "moved")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	parent, leaf, err := openSharedParent(root, "owned/result.dat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parent)
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, original); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Openat(parent, leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	if _, err := os.Stat(filepath.Join(moved, leaf)); err != nil {
		t.Fatalf("operation did not remain on pinned parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, leaf)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operation escaped through replaced parent: %v", err)
	}
}

func TestSecureSharedPathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := secureSharedPath(root, "escape/secret", true); err == nil {
		t.Fatal("symlink escape must be rejected")
	}
}

func TestDecodeRelativePathAcceptsUnicodeAndRejectsTraversal(t *testing.T) {
	encoded := base64.RawURLEncoding.EncodeToString([]byte("数据/input.tpr"))
	if path, err := decodeRelativePath(encoded); err != nil || path != "数据/input.tpr" {
		t.Fatalf("unexpected decoded path %q: %v", path, err)
	}
	traversal := base64.RawURLEncoding.EncodeToString([]byte("../secret"))
	if _, err := decodeRelativePath(traversal); err == nil {
		t.Fatal("encoded traversal must be rejected")
	}
}

func TestDirectoryUsedBytesCountsOnlyRegularFiles(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if used, err := directoryUsedBytes(root); err != nil || used != 0 {
		t.Fatalf("empty directory size = %d, %v; want 0", used, err)
	}
	if err := os.WriteFile(filepath.Join(root, "one.bin"), make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "two.bin"), make([]byte, 31), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, make([]byte, 99), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Fatal(err)
	}
	used, err := directoryUsedBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	if used != 48 {
		t.Fatalf("directory size = %d; want 48", used)
	}
}

func TestSafeMountPathRejectsTraversalAndUncleanPaths(t *testing.T) {
	for _, value := range []string{
		"/srv/xirang/shared",
		"/srv/x_1/share-2/data.d",
	} {
		if !safeMountPath(value) {
			t.Errorf("rejected a legitimate absolute path: %s", value)
		}
	}
	for _, value := range []string{
		"/srv/share/../../etc", // escapes the intended root
		"/srv/share/..",        // escapes by one level
		"/../etc/shadow",       // escapes above the root
		"/srv//share",          // unclean, not what the caller reviewed
		"/srv/share/",          // trailing separator
		"/srv/./share",         // unclean
		"..",                   // not absolute
		"/",                    // the whole filesystem
		"relative/path",        // not absolute
		"/srv/share;rm -rf /",  // outside the character set
		"/srv/share$(id)",      // outside the character set
	} {
		if safeMountPath(value) {
			t.Errorf("accepted an unsafe path at the root privilege boundary: %s", value)
		}
	}
}
