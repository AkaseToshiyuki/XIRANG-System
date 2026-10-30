package build

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xirang-grid/xirang/internal/safefile"
	"golang.org/x/sys/unix"
)

// publishBuildOutput keeps existing host directories intact. New directories
// and files share the configured task group even under a restrictive umask.
// Walk and publish relative to pinned descriptors so a replaced symlink cannot
// redirect ownership changes or the final rename outside the selected path.
func (service *Service) publishBuildOutput(source, destination string) error {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return ErrInvalidRequest
	}
	input, err := safefile.OpenRegular(filepath.Dir(source), filepath.Base(source))
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	uid, gid := -1, -1
	if service.sharedWorkspace {
		uid, gid = service.sharedUID, service.sharedGID
	}
	parent, err := openPublicationParent(filepath.Dir(destination), uid, gid)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	name := ".xirang-publish-" + hex.EncodeToString(id[:])
	fd, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(parent, name, 0)
	output := os.NewFile(uintptr(fd), name)
	defer output.Close()
	if uid >= 0 && gid >= 0 {
		if err := output.Chown(uid, gid); err != nil {
			return err
		}
	}
	mode := os.FileMode(0o440)
	if info.Mode().Perm()&0o111 != 0 {
		mode = 0o550
	}
	if err := output.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return unix.Renameat(parent, name, parent, filepath.Base(destination))
}

func openPublicationParent(path string, uid, gid int) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			created := unix.Mkdirat(fd, part, 0o700)
			if created != nil && !errors.Is(created, unix.EEXIST) {
				unix.Close(fd)
				return -1, created
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if openErr == nil && created == nil {
				if uid >= 0 && gid >= 0 {
					openErr = unix.Fchown(next, uid, gid)
				}
				if openErr == nil {
					openErr = unix.Fchmod(next, 0o2770)
				}
				if openErr != nil {
					unix.Close(next)
				}
			}
		}
		unix.Close(fd)
		if openErr != nil {
			return -1, openErr
		}
		fd = next
	}
	return fd, nil
}
