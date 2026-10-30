package agent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux's POSIX ACL xattr ABI (linux/posix_acl{,_xattr}.h). Use descriptors
// directly: setfacl can fall back to chmod on filesystems without ACL support,
// which would alter host permissions even when it reports failure.
const (
	sharedAccessACL        = "system.posix_acl_access"
	aclUserObject   uint16 = 0x01
	aclUser         uint16 = 0x02
	aclGroupObject  uint16 = 0x04
	aclGroup        uint16 = 0x08
	aclMask         uint16 = 0x10
	aclOther        uint16 = 0x20
	aclNoID         uint32 = ^uint32(0)
)

type sharedACLEntry struct {
	tag, perm uint16
	id        uint32
}

func decodeSharedACL(data []byte) ([]sharedACLEntry, error) {
	if len(data) < 4 || (len(data)-4)%8 != 0 || binary.LittleEndian.Uint32(data) != 2 {
		return nil, errors.New("invalid POSIX ACL encoding")
	}
	entries := make([]sharedACLEntry, 0, (len(data)-4)/8)
	for offset := 4; offset < len(data); offset += 8 {
		entry := sharedACLEntry{binary.LittleEndian.Uint16(data[offset:]), binary.LittleEndian.Uint16(data[offset+2:]), binary.LittleEndian.Uint32(data[offset+4:])}
		if entry.perm > 7 {
			return nil, errors.New("invalid POSIX ACL permissions")
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func encodeSharedACL(entries []sharedACLEntry) []byte {
	data := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(data, 2)
	for index, entry := range entries {
		offset := 4 + 8*index
		binary.LittleEndian.PutUint16(data[offset:], entry.tag)
		binary.LittleEndian.PutUint16(data[offset+2:], entry.perm)
		binary.LittleEndian.PutUint32(data[offset+4:], entry.id)
	}
	return data
}

func readSharedACL(fd int) ([]byte, error) {
	// Linux caps the complete xattr value at 64 KiB. One read also avoids a
	// size/read race if an administrator changes the ACL concurrently.
	data := make([]byte, 65536)
	n, err := unix.Fgetxattr(fd, sharedAccessACL, data)
	if errors.Is(err, unix.ENODATA) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}

// grantSharedDirectoryAccess adds access for the cluster identity and task
// group, without replacing the host owner/group or changing other identities'
// effective access. Default ACLs and descendants are deliberately untouched.
func grantSharedDirectoryAccess(fd int, uid, gid int) error {
	if uid < 0 || gid < 0 || uint64(uid) >= uint64(aclNoID) || uint64(gid) >= uint64(aclNoID) {
		return errors.New("invalid shared-space identity")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("shared-space source must be a directory")
	}
	original, err := readSharedACL(fd)
	if err != nil {
		return fmt.Errorf("shared-space permissions require POSIX ACL support: %w", err)
	}
	entries := []sharedACLEntry{
		{aclUserObject, uint16(stat.Mode>>6) & 7, aclNoID},
		{aclGroupObject, uint16(stat.Mode>>3) & 7, aclNoID},
		{aclOther, uint16(stat.Mode) & 7, aclNoID},
	}
	if len(original) != 0 {
		entries, err = decodeSharedACL(original)
		if err != nil {
			return err
		}
	}
	mask := uint16(7)
	for _, entry := range entries {
		if entry.tag == aclMask {
			mask = entry.perm
		}
	}
	updated := make([]sharedACLEntry, 0, len(entries)+2)
	groupAdded := false
	for _, entry := range entries {
		// Raising the mask must not reactivate permissions which were masked
		// off for an existing host user/group.
		if entry.tag == aclUser || entry.tag == aclGroupObject || entry.tag == aclGroup {
			entry.perm &= mask
		}
		if entry.tag == aclUser && entry.id == uint32(uid) || entry.tag == aclUserObject && stat.Uid == uint32(uid) {
			entry.perm = 7
		}
		if entry.tag == aclMask {
			continue
		}
		if !groupAdded && (entry.tag > aclGroup || entry.tag == aclGroup && entry.id >= uint32(gid)) {
			updated = append(updated, sharedACLEntry{aclGroup, 7, uint32(gid)})
			groupAdded = true
		}
		if entry.tag == aclGroup && entry.id == uint32(gid) {
			continue
		}
		if entry.tag == aclOther {
			updated = append(updated, sharedACLEntry{aclMask, 7, aclNoID})
		}
		updated = append(updated, entry)
	}
	encoded := encodeSharedACL(updated)
	if bytes.Equal(original, encoded) {
		return nil
	}
	if err := unix.Fsetxattr(fd, sharedAccessACL, encoded, 0); err != nil {
		return fmt.Errorf("cannot grant cluster access without changing host permissions (POSIX ACL required): %w", err)
	}
	return nil
}

// openOrCreateSharedDirectory never chmods existing ancestors and rejects
// symlinks at every component. All metadata changes use the pinned descriptor.
func openOrCreateSharedDirectory(path string) (*os.File, error) {
	if !safeMountPath(path) {
		return nil, errors.New("invalid shared directory path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, openErr := openSharedAt(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
		if errors.Is(openErr, unix.ENOENT) {
			mkdirErr := unix.Mkdirat(fd, part, 0o755)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				unix.Close(fd)
				return nil, mkdirErr
			}
			next, openErr = openSharedAt(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
			if openErr == nil && mkdirErr == nil {
				var created unix.Stat_t
				openErr = unix.Fstat(next, &created)
				if openErr == nil {
					openErr = unix.Fchmod(next, 0o755|(created.Mode&unix.S_ISGID))
				}
				if openErr != nil {
					unix.Close(next)
				}
			}
		}
		unix.Close(fd)
		if openErr != nil {
			return nil, openErr
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

func prepareSharedDirectory(path string, uid, gid int) error {
	directory, err := openOrCreateSharedDirectory(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return grantSharedDirectoryAccess(int(directory.Fd()), uid, gid)
}

func createSharedSubdirectory(parent int, name string, uid, gid int, allowExisting bool) (int, error) {
	err := unix.Mkdirat(parent, name, 0o770)
	if err != nil && (!allowExisting || !errors.Is(err, unix.EEXIST)) {
		return -1, err
	}
	fd, openErr := openSharedAt(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if openErr != nil {
		return -1, openErr
	}
	// An existing host directory is not ours to chown or claim in the file API.
	if err != nil {
		return fd, nil
	}
	if err := unix.Fchown(fd, uid, gid); err != nil {
		unix.Close(fd)
		return -1, err
	}
	// A restrictive service umask must not remove task-group access. The ACL
	// merge also preserves effective permissions inherited from a default ACL.
	if err := grantSharedDirectoryAccess(fd, uid, gid); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}
