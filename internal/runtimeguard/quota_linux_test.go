package runtimeguard

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestManagedWorkloadCannotChangeQuotaAttributes(t *testing.T) {
	if os.Getenv("XIRANG_QUOTA_GUARD_CHILD") == "1" {
		if err := ProtectQuotaMetadata(); err != nil {
			t.Fatal(err)
		}
		for _, command := range []uintptr{0x401c5820, 0x40086602, 0x40046602} {
			_, _, err := unix.Syscall(unix.SYS_IOCTL, ^uintptr(0), command, 0)
			if err != unix.EPERM {
				t.Fatalf("quota ioctl %x returned %v", command, err)
			}
		}
		_, _, err := unix.Syscall6(unix.SYS_FILE_SETATTR, ^uintptr(0), 0, 0, 0, 0, 0)
		if err != unix.EPERM {
			t.Fatalf("file_setattr returned %v", err)
		}
		_, _, err = unix.Syscall(unix.SYS_IOCTL, ^uintptr(0), unix.TIOCGWINSZ, 0)
		if err != unix.EBADF {
			t.Fatalf("unrelated ioctl unexpectedly restricted: %v", err)
		}
		if output, err := exec.Command("/bin/sh", "-c", "printf allowed").Output(); err != nil || string(output) != "allowed" {
			t.Fatalf("ordinary exec: %q %v", output, err)
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestManagedWorkloadCannotChangeQuotaAttributes$")
	command.Env = append(os.Environ(), "XIRANG_QUOTA_GUARD_CHILD=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("guard subprocess failed: %s %v", output, err)
	}
}
