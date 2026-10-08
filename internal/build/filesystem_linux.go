//go:build linux

package build

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func buildFilesystemABI() error {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return fmt.Errorf("native build filesystem isolation requires Landlock ABI 3: %w", errno)
	}
	if abi < 3 {
		return errors.New("native build filesystem isolation requires Landlock ABI 3 (Linux 6.2 or newer)")
	}
	return nil
}

// Constrain the compiler even when NFS all_squash or shared task groups grant
// DAC access to other builds' staged sources. Rules are inherited across exec.
func restrictBuildFilesystem(workspace string) error {
	if err := buildFilesystemABI(); err != nil {
		return err
	}
	const handled = uint64((1 << 15) - 1) // ABI 3: EXECUTE through TRUNCATE.
	const read = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), 8, 0)
	if errno != 0 {
		return errno
	}
	defer unix.Close(int(fd))
	add := func(path string, access uint64, optional bool) error {
		parent, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if optional && errors.Is(err, unix.ENOENT) {
			return nil
		}
		if err != nil {
			return err
		}
		defer unix.Close(parent)
		var st unix.Stat_t
		if err := unix.Fstat(parent, &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			access &= unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE
		}
		rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(parent)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		if errno != 0 {
			return fmt.Errorf("build filesystem rule %s: %w", path, errno)
		}
		return nil
	}
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/proc", "/sys/devices/system/cpu"} {
		resolved, err := filepath.EvalSymlinks(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(resolved, workspace)
		if err != nil {
			return err
		}
		if relative == "." || (relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, "../")) {
			return errors.New("build workspace must be outside system read-only paths")
		}
		if err := add(resolved, read, true); err != nil {
			return err
		}
	}
	if err := add(workspace, handled, false); err != nil {
		return err
	}
	for _, path := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom"} {
		if err := add(path, unix.LANDLOCK_ACCESS_FS_READ_FILE|unix.LANDLOCK_ACCESS_FS_WRITE_FILE, true); err != nil {
			return err
		}
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
