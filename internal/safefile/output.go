// Package safefile opens untrusted build outputs without resolving links.
package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenRegular pins root and each directory component, then opens and checks
// the leaf descriptor. root's parent is trusted configuration; root itself and
// everything beneath it may have been changed by the build identity.
func OpenRegular(root, relative string) (*os.File, error) {
	return openRegular(root, relative, unix.O_RDONLY|unix.O_NONBLOCK)
}

// OpenRegularForMetadata permits reclaiming an output whose build UID owns
// mode 0700, without requiring the controller to bypass its read permissions.
func OpenRegularForMetadata(root, relative string) (*os.File, error) {
	return openRegular(root, relative, unix.O_PATH)
}

func openRegular(root, relative string, flags int) (*os.File, error) {
	relative = filepath.Clean(filepath.FromSlash(relative))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("output path must remain inside its workspace")
	}
	fd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	leaf, err := unix.Openat(fd, parts[len(parts)-1], flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	_ = unix.Close(fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(leaf), filepath.Join(root, relative))
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("build output must be a regular file")
	}
	return file, nil
}
