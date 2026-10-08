//go:build linux

package build

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBuildFilesystemBlocksOtherStaging(t *testing.T) {
	if workspace := os.Getenv("XIRANG_FS_TEST_WORKSPACE"); workspace != "" {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := restrictBuildFilesystem(workspace); err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(workspace); err != nil {
			t.Fatal(err)
		}
		script := `set -eu
if cat "$1/secret" 2>/dev/null; then exit 11; fi
if ls "$1" 2>/dev/null; then exit 12; fi
if (echo changed > "$1/secret") 2>/dev/null; then exit 13; fi
if truncate -s 0 "$1/secret" 2>/dev/null; then exit 17; fi
if rm "$1/secret" 2>/dev/null; then exit 14; fi
if cat outside-link 2>/dev/null; then exit 15; fi
if cat "/proc/self/root$1/secret" 2>/dev/null; then exit 16; fi
cc source.c -o app
./app
`
		if err := syscall.Exec("/bin/sh", []string{"sh", "-c", script, "sh", os.Getenv("XIRANG_FS_TEST_OTHER")}, append(os.Environ(), "TMPDIR="+workspace)); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := buildFilesystemABI(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "current")
	other := filepath.Join(root, "staged")
	for _, path := range []string{workspace, other} {
		if err := os.Mkdir(path, 0770); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(other, "secret"), []byte("another tenant's secret"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "source.c"), []byte("int main(void) { return 0; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "secret"), filepath.Join(workspace, "outside-link")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBuildFilesystemBlocksOtherStaging$")
	cmd.Env = append(os.Environ(), "XIRANG_FS_TEST_WORKSPACE="+workspace, "XIRANG_FS_TEST_OTHER="+other)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("filesystem confinement: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(other, "secret"))
	if err != nil || string(data) != "another tenant's secret" {
		t.Fatalf("staged secret changed: %q %v", data, err)
	}
}
