package agent

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func aclTool(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skip("independent ACL reference requires " + name)
	}
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v %s", name, err, output)
	}
	return output
}

func TestSharedExportPreservesHostPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0700, 0755, 0775, os.ModeSticky | 0777, os.ModeSetgid | 0770} {
		t.Run(mode.String(), func(t *testing.T) {
			path := t.TempDir()
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(path, "host.txt")
			if err := os.WriteFile(child, []byte("host data"), 0640); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			if err := prepareSharedDirectory(path, 400000001, 400000002); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(path)
			oldStat, newStat := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
			if oldStat.Uid != newStat.Uid || oldStat.Gid != newStat.Gid || before.Mode()&(os.ModeSetgid|os.ModeSticky|0707) != after.Mode()&(os.ModeSetgid|os.ModeSticky|0707) {
				t.Fatalf("host ownership, owner/other or special bits changed: %v -> %v", before, after)
			}
			acl := aclTool(t, "getfacl", "-c", "-p", "-n", path)
			if !bytes.Contains(acl, []byte("group:400000002:rwx")) {
				t.Fatalf("cluster group missing: %s", acl)
			}
			// getfacl independently decodes the kernel ACL; the owning host
			// group's entry must retain the original group permissions.
			perm := before.Mode().Perm()
			want := "group::" + permissionText(uint16(perm>>3)&7) + "\n"
			if !bytes.Contains(acl, []byte(want)) {
				t.Fatalf("host group changed: %s want %s", acl, want)
			}
			if err := prepareSharedDirectory(path, 400000001, 400000002); err != nil {
				t.Fatal(err)
			}
			if again := aclTool(t, "getfacl", "-c", "-p", "-n", path); !bytes.Equal(acl, again) {
				t.Fatalf("not idempotent: %s -> %s", acl, again)
			}
			info, _ := os.Stat(child)
			if info.Mode().Perm() != 0640 {
				t.Fatal("export changed a descendant")
			}
		})
	}
}

func permissionText(perm uint16) string {
	value := []byte("---")
	for i, bit := range []uint16{4, 2, 1} {
		if perm&bit != 0 {
			value[i] = "rwx"[i]
		}
	}
	return string(value)
}

func TestSharedExportPreservesEffectiveAndDefaultACLs(t *testing.T) {
	path := t.TempDir()
	aclTool(t, "setfacl", "-m", "u::rwx,u:400000003:rwx,g::rwx,g:400000004:rwx,m::r-x,o::--x", path)
	aclTool(t, "setfacl", "-d", "-m", "u::rwx,u:400000003:rwx,g::rwx,m::r-x,o::---", path)
	defaultBefore := aclTool(t, "getfacl", "-c", "-p", "-n", "-d", path)
	if err := prepareSharedDirectory(path, 400000001, 400000002); err != nil {
		t.Fatal(err)
	}
	acl := string(aclTool(t, "getfacl", "-c", "-p", "-n", "-a", path))
	want := "user::rwx\nuser:400000003:r-x\ngroup::r-x\ngroup:400000002:rwx\ngroup:400000004:r-x\nmask::rwx\nother::--x\n\n"
	if acl != want {
		t.Fatalf("masked permissions changed: got %s want %s", acl, want)
	}
	if after := aclTool(t, "getfacl", "-c", "-p", "-n", "-d", path); !bytes.Equal(defaultBefore, after) {
		t.Fatal("default ACL changed")
	}
}

func TestSharedExportUpdatesExistingTaskEntries(t *testing.T) {
	path := t.TempDir()
	aclTool(t, "setfacl", "-m", "u:400000001:r--,g:400000002:r--,m::r--", path)
	if err := prepareSharedDirectory(path, 400000001, 400000002); err != nil {
		t.Fatal(err)
	}
	acl := aclTool(t, "getfacl", "-c", "-p", "-n", path)
	for _, want := range []string{"user:400000001:rwx", "group:400000002:rwx"} {
		if !bytes.Contains(acl, []byte(want)) {
			t.Fatalf("task entry was not granted: %s", acl)
		}
	}
}

func TestSharedDirectoryPreparationPreservesAncestorsAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, os.ModeSetgid|0770); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "parent", "share")
	if err := prepareSharedDirectory(path, 400000001, 400000002); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(root)
	if info.Mode().Perm() != 0770 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatal("existing parent permissions changed")
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm()&7 != 5 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatal("new share lost host readability or inherited setgid policy")
	}
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{link, filepath.Join(link, "new")} {
		if err := prepareSharedDirectory(target, 400000001, 400000002); err == nil {
			t.Fatal("accepted symlink")
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("modified symlink target")
	}
}

func TestSharedDirectoryPermissionsUsePinnedDescriptor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "share")
	directory, err := openOrCreateSharedDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	before := aclTool(t, "getfacl", "-c", "-p", "-n", outside)
	if err := grantSharedDirectoryAccess(int(directory.Fd()), 400000001, 400000002); err != nil {
		t.Fatal(err)
	}
	if after := aclTool(t, "getfacl", "-c", "-p", "-n", outside); !bytes.Equal(before, after) {
		t.Fatal("changed symlink target")
	}
	if after := aclTool(t, "getfacl", "-c", "-p", "-n", path+"-moved"); !bytes.Contains(after, []byte("group:400000002:rwx")) {
		t.Fatal("pinned directory unchanged")
	}
}

func TestSharedSubdirectorySurvivesRestrictiveUmask(t *testing.T) {
	root := t.TempDir()
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	previous := unix.Umask(0077)
	defer unix.Umask(previous)
	fd, err := createSharedSubdirectory(int(parent.Fd()), "nested", os.Geteuid(), os.Getegid(), false)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	acl := aclTool(t, "getfacl", "-c", "-p", "-n", filepath.Join(root, "nested"))
	if !bytes.Contains(acl, []byte("mask::rwx")) {
		t.Fatalf("umask removed task-group write: %s", acl)
	}
	// Retrying a raced mkdir must not take ownership of an existing directory.
	if err := os.Chmod(filepath.Join(root, "nested"), 0751); err != nil {
		t.Fatal(err)
	}
	before := aclTool(t, "getfacl", "-c", "-p", "-n", filepath.Join(root, "nested"))
	if fd, err := createSharedSubdirectory(int(parent.Fd()), "nested", 400000001, 400000002, false); !errors.Is(err, unix.EEXIST) {
		if fd >= 0 {
			unix.Close(fd)
		}
		t.Fatalf("file-mkdir claimed an existing directory: %v", err)
	}
	fd, err = createSharedSubdirectory(int(parent.Fd()), "nested", 400000001, 400000002, true)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	if after := aclTool(t, "getfacl", "-c", "-p", "-n", filepath.Join(root, "nested")); !bytes.Equal(before, after) {
		t.Fatal("existing directory changed")
	}
}

func TestNestedSharedDirectoryListingUsesDescriptor(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "nested", "child")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "data.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	entries, err := listSharedDirectory(root, "nested/child")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "nested/child/data.txt" || entries[0].SizeBytes != 5 || entries[0].Directory {
		t.Fatalf("nested listing: %#v", entries)
	}
}

// Run this test as root in an isolated CI fixture to verify actual kernel
// access checks with three distinct credentials, not just ACL serialization.
func TestSharedDirectoryHostAccessProcesses(t *testing.T) {
	if path := os.Getenv("XIRANG_ACL_TEST_PATH"); path != "" {
		if _, err := os.ReadFile(filepath.Join(path, "host.txt")); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("XIRANG_ACL_TEST_WRITE") == "yes" {
			if err := os.WriteFile(filepath.Join(path, "cluster.txt"), []byte("cluster"), 0660); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(path, "forbidden.txt"), nil, 0666); err == nil {
			t.Fatal("host read-only identity gained write access")
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("cross-identity filesystem access requires root")
	}
	root, err := os.MkdirTemp("", "xirang-acl-process-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	// The original go-test executable may live below a private build directory.
	executable, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	testBinary := filepath.Join(root, "access.test")
	if err := os.WriteFile(testBinary, executable, 0555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "host.txt"), []byte("host"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareSharedDirectory(root, 400000001, 400000002); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		uid, gid uint32
		groups   []uint32
		write    string
	}{
		{400000003, 400000004, []uint32{}, "no"},
		{400000001, 400000002, []uint32{}, "yes"},
		{400000005, 400000006, []uint32{400000002}, "yes"},
	} {
		command := exec.Command(testBinary, "-test.run=^TestSharedDirectoryHostAccessProcesses$")
		command.Env = append(os.Environ(), "XIRANG_ACL_TEST_PATH="+root, "XIRANG_ACL_TEST_WRITE="+tc.write)
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: tc.uid, Gid: tc.gid, Groups: tc.groups}}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("uid %d: %v %s", tc.uid, err, output)
		}
		os.Remove(filepath.Join(root, "cluster.txt"))
	}
}

func TestSharedACLRejectsMalformedData(t *testing.T) {
	for _, data := range [][]byte{nil, {1, 0, 0, 0}, {2, 0, 0, 0, 1}, {2, 0, 0, 0, 1, 0, 8, 0, 255, 255, 255, 255}} {
		if _, err := decodeSharedACL(data); err == nil {
			t.Fatalf("accepted malformed ACL: %x", data)
		}
	}
	file, err := os.CreateTemp(t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := grantSharedDirectoryAccess(int(file.Fd()), 400000001, 400000002); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("accepted non-directory: %v", err)
	}
}

func TestSharedACLUnsupportedFilesystemFailsWithoutChmod(t *testing.T) {
	// procfs rejects POSIX ACLs. Exercise the real kernel failure, not a fake
	// setter which could conceal a destructive chmod fallback.
	directory, err := os.Open("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	before, err := directory.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := grantSharedDirectoryAccess(int(directory.Fd()), 400000001, 400000002); err == nil {
		t.Fatal("unsupported ACL was accepted")
	}
	after, err := directory.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Fatal("failed ACL operation changed permissions")
	}
}
