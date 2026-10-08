//go:build linux

package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// All native workers on a host must use this same administrator-owned namespace.
const buildLockRoot = "/var/lib/xirang-build/locks"

func (runner *NativeRunner) openIdentityLock() (*os.File, error) {
	root := runner.lockRoot
	if root == "" {
		root = buildLockRoot
	}
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open build identity lock directory (run the installer): %w", err)
	}
	defer unix.Close(dir)
	var st unix.Stat_t
	if err = unix.Fstat(dir, &st); err != nil {
		return nil, err
	}
	if st.Uid == runner.credential.Uid || st.Mode&0002 != 0 {
		return nil, errors.New("build identity can modify lock directory")
	}
	groups := append([]uint32{runner.credential.Gid}, runner.credential.Groups...)
	for _, gid := range groups {
		if st.Gid == gid && st.Mode&0020 != 0 {
			return nil, errors.New("build group can modify lock directory")
		}
	}
	name := strconv.FormatUint(uint64(runner.credential.Uid), 10) + ".lock"
	// Installer creates the shared file before either daemon starts; custom UIDs
	// can also create it here when both daemons use the directory's service group.
	fd, err := unix.Openat(dir, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0660)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root, name))
	if err = unix.Fstat(fd, &st); err == nil && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid == runner.credential.Uid || st.Nlink != 1 || st.Mode&0002 != 0) {
		err = errors.New("unsafe build identity lock file")
	}
	for _, gid := range groups {
		if err == nil && st.Gid == gid && st.Mode&0020 != 0 {
			err = errors.New("build group can modify lock file")
		}
	}
	if err == nil && int(st.Uid) == os.Geteuid() {
		var dst unix.Stat_t
		err = unix.Fstat(dir, &dst)
		if err == nil {
			err = file.Chown(-1, int(dst.Gid))
		}
		if err == nil {
			err = file.Chmod(0660)
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (runner *NativeRunner) AcquireBuild(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := runner.openIdentityLock()
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	fail := func(err error) (func(), error) { file.Close(); return nil, err }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if info.Size() != 0 {
		return fail(errors.New("build identity quarantined after incomplete cleanup; administrator recovery required"))
	}
	// Leave a durable marker before exposing any workspace. A daemon crash
	// requires recovery too; flock alone disappears when its process exits.
	if _, err := file.WriteAt([]byte("active build; cleanup not confirmed\n"), 0); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	runner.leaseMu.Lock()
	runner.lease = file
	runner.leaseMu.Unlock()
	return func() {
		runner.leaseMu.Lock()
		defer runner.leaseMu.Unlock()
		if runner.lease != file {
			return
		}
		if runner.quarantine != nil {
			// Persist the quarantine across daemon restarts. If persistence fails,
			// retain the locked descriptor so no other worker can reuse the identity.
			if _, err := file.WriteAt([]byte(runner.quarantine.Error()+"\n"), 0); err != nil {
				return
			}
			if err := file.Sync(); err != nil {
				return
			}
		} else {
			if err := file.Truncate(0); err != nil {
				return
			}
			if err := file.Sync(); err != nil {
				return
			}
		}
		file.Close()
		runner.lease = nil
	}, nil
}

func (runner *NativeRunner) quarantineIdentity(err error) {
	runner.leaseMu.Lock()
	defer runner.leaseMu.Unlock()
	runner.quarantine = errors.Join(runner.quarantine, err)
}

func (runner *NativeRunner) CleanupWorkspace(workspace string) error {
	var err error
	if !runner.shared {
		err = reclaimBuildDirectory(unix.AT_FDCWD, workspace)
	}
	if err == nil {
		err = os.RemoveAll(workspace)
	}
	if err != nil {
		runner.quarantineIdentity(err)
	}
	return err
}

// Reclaim only pinned directories; symlinks and regular files are unlinked by
// their containing directory. CAP_CHOWN suffices even for mode-000 directories.
func reclaimBuildDirectory(parent int, name string) error {
	fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Fchownat(fd, "", os.Geteuid(), os.Getegid(), unix.AT_EMPTY_PATH); err != nil {
		return err
	}
	if err = os.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), 0700); err != nil {
		return err
	}
	readFD, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(readFD), name)
	defer file.Close()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, child := range names {
		var st unix.Stat_t
		if err = unix.Fstatat(fd, child, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err = reclaimBuildDirectory(fd, child); err != nil {
				return err
			}
		}
	}
	return nil
}
