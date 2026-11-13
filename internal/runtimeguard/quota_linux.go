// Package runtimeguard contains restrictions inherited by managed workloads.
package runtimeguard

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ProtectQuotaMetadata prevents an unprivileged inode owner from clearing its
// project ID or inheritance flag. It leaves ordinary file and device I/O intact.
// The caller must immediately exec the workload on this locked OS thread.
func ProtectQuotaMetadata() error {
	var architecture uint32
	switch runtime.GOARCH {
	case "amd64":
		architecture = unix.AUDIT_ARCH_X86_64
	case "arm64":
		architecture = unix.AUDIT_ARCH_AARCH64
	default:
		return errors.New("quota metadata protection requires Linux amd64 or arm64")
	}
	runtime.LockOSThread()
	fail := func(err error) error { runtime.UnlockOSThread(); return err }
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fail(err)
	}
	deny := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4}, // seccomp_data.arch
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: architecture, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},                  // syscall number
		{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: 0x40000000, Jf: 1}, // deny x32 ABI
		{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_FILE_SETATTR, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_IOCTL, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 24},                 // ioctl request, low 32 bits
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0x401c5820, Jt: 3}, // FS_IOC_FSSETXATTR
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0x40086602, Jt: 2}, // FS_IOC_SETFLAGS
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0x40046602, Jt: 1}, // 32-bit SETFLAGS
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		{Code: unix.BPF_RET | unix.BPF_K, K: deny},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := unix.Syscall(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fail(fmt.Errorf("protect managed project quota metadata: %w", errno))
	}
	return nil
}
