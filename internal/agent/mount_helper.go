package agent

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RunMountHelper performs the narrow root operations used by the controller's
// shared-space manager. It accepts only absolute, shell-free paths.
func RunMountHelper(arguments []string) error {
	if os.Geteuid() != 0 {
		return errors.New("mount-helper must run as root")
	}
	if len(arguments) < 3 || !safeMountID(arguments[1]) {
		return errors.New("usage: mount-helper <action> <id> [arguments]")
	}
	action, id := arguments[0], arguments[1]
	switch action {
	case "policy-authorize":
		if len(arguments) != 3 || !safeMountPath(arguments[2]) {
			return errors.New("policy-authorize requires id and a safe absolute path")
		}
		return updateSystemdSharedPathPolicy(id, arguments[2], true)
	case "policy-revoke":
		if len(arguments) != 3 || !safeMountPath(arguments[2]) {
			return errors.New("policy-revoke requires id and a safe absolute path")
		}
		return updateSystemdSharedPathPolicy(id, arguments[2], false)
	case "stats":
		if len(arguments) != 3 || !safeMountPath(arguments[2]) {
			return errors.New("stats requires a safe absolute source path")
		}
		var statistics syscall.Statfs_t
		if err := syscall.Statfs(arguments[2], &statistics); err != nil {
			return err
		}
		blockSize := uint64(statistics.Bsize)
		total := statistics.Blocks * blockSize
		available := statistics.Bavail * blockSize
		used, err := directoryUsedBytes(arguments[2])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, `{"usedBytes":%d,"availableBytes":%d,"totalBytes":%d}`+"\n", used, available, total)
		return err
	case "export":
		if len(arguments) != 3 || !safeMountPath(arguments[2]) {
			return errors.New("export requires a safe absolute source path")
		}
		if err := os.MkdirAll(arguments[2], 0o770); err != nil {
			return err
		}
		taskIdentity, err := user.Lookup("xirang-task")
		if err != nil {
			return errors.New("xirang-task identity is required")
		}
		uid, uidErr := strconv.Atoi(taskIdentity.Uid)
		gid, gidErr := strconv.Atoi(taskIdentity.Gid)
		if uidErr != nil || gidErr != nil {
			return errors.New("xirang-task identity has invalid numeric IDs")
		}
		if err := os.Chown(arguments[2], uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(arguments[2], 0o770); err != nil {
			return err
		}
		path := filepath.Join("/etc/exports.d", "xirang-"+id+".exports")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		exportLine := fmt.Sprintf("%s *(rw,sync,no_subtree_check,all_squash,anonuid=%d,anongid=%d)\n", arguments[2], uid, gid)
		if err := os.WriteFile(path, []byte(exportLine), 0o644); err != nil {
			return err
		}
		return runMountCommand("exportfs", "-ra")
	case "unexport":
		if len(arguments) != 3 || !safeMountPath(arguments[2]) {
			return errors.New("unexport requires a safe absolute source path")
		}
		if err := os.Remove(filepath.Join("/etc/exports.d", "xirang-"+id+".exports")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return runMountCommand("exportfs", "-ra")
	case "mount":
		if len(arguments) != 5 || !safeMountHost(arguments[2]) || !safeMountPath(arguments[3]) || !safeMountPath(arguments[4]) {
			return errors.New("mount requires id, source host, source path, and mount path")
		}
		source, target := arguments[2]+":"+arguments[3], arguments[4]
		if err := updateSystemdSharedPathPolicy(id+"-mount", target, true); err != nil {
			return err
		}
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		// Mount roots such as /shared must be traversable by the task identity.
		// The mounted export itself retains its own, stricter permissions.
		if err := os.Chmod(parent, 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := runMountCommand("mountpoint", "-q", target); err != nil {
			if err := runMountCommand("mount", "-t", "nfs", "-o", "rw,hard,timeo=60,retrans=2", source, target); err != nil {
				return err
			}
		}
		return putFSTab(id, source, target)
	case "unmount":
		if len(arguments) != 3 || !safeMountPath(arguments[2]) {
			return errors.New("unmount requires id and mount path")
		}
		if err := runMountCommand("mountpoint", "-q", arguments[2]); err == nil {
			if err := runMountCommand("umount", arguments[2]); err != nil {
				return err
			}
		}
		if err := removeFSTab(id); err != nil {
			return err
		}
		return updateSystemdSharedPathPolicy(id+"-mount", arguments[2], false)
	case "file-list":
		root, relative, err := fileArguments(arguments, 4)
		if err != nil {
			return err
		}
		directory, err := openSharedDirectory(root, relative)
		if err != nil {
			return err
		}
		defer directory.Close()
		entries, err := directory.ReadDir(-1)
		if err != nil {
			return err
		}
		result := make([]sharedFileEntry, 0, len(entries))
		for _, entry := range entries {
			info, infoErr := entry.Info()
			if infoErr != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			child := filepath.ToSlash(filepath.Join(relative, entry.Name()))
			result = append(result, sharedFileEntry{
				Path: child, Name: entry.Name(), Directory: entry.IsDir(),
				SizeBytes: uint64(maxInt64(info.Size(), 0)), ModifiedAt: info.ModTime().UTC(),
				Owner: fileOwner(info),
			})
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "file-read":
		root, relative, err := fileArguments(arguments, 4)
		if err != nil || relative == "" {
			return errors.New("file-read requires a file path")
		}
		file, err := openSharedRegularFile(root, relative)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(os.Stdout, file)
		return err
	case "file-write":
		if len(arguments) != 6 {
			return errors.New("file-write requires a file path, write mode, and byte limit")
		}
		root, relative, err := fileArguments(arguments[:4], 4)
		mode := arguments[4]
		maximumBytes, limitErr := strconv.ParseInt(arguments[5], 10, 64)
		if err != nil || limitErr != nil || maximumBytes < 1 || maximumBytes > 64<<30 || relative == "" || (mode != "create" && mode != "replace" && mode != "upsert") {
			return errors.New("file-write requires a file path")
		}
		parentFD, leaf, err := openSharedParent(root, relative, true)
		if err != nil {
			return err
		}
		defer unix.Close(parentFD)
		var existing unix.Stat_t
		existingErr := unix.Fstatat(parentFD, leaf, &existing, unix.AT_SYMLINK_NOFOLLOW)
		switch mode {
		case "create":
			if existingErr == nil {
				return errors.New("destination already exists")
			}
			if !errors.Is(existingErr, unix.ENOENT) {
				return existingErr
			}
		case "replace":
			if existingErr != nil {
				return existingErr
			}
			if existing.Mode&unix.S_IFMT != unix.S_IFREG {
				return errors.New("file-write can only replace a regular file")
			}
		case "upsert":
			if existingErr == nil && existing.Mode&unix.S_IFMT != unix.S_IFREG {
				return errors.New("file-write can only replace a regular file")
			}
			if existingErr != nil && !errors.Is(existingErr, unix.ENOENT) {
				return existingErr
			}
		}
		temporary, err := os.CreateTemp(procDirectoryPath(parentFD), ".xirang-upload-*")
		if err != nil {
			return err
		}
		temporaryName := temporary.Name()
		defer os.Remove(temporaryName)
		written, copyErr := io.Copy(temporary, io.LimitReader(os.Stdin, maximumBytes+1))
		if copyErr != nil {
			err = copyErr
		} else if written > maximumBytes {
			err = errors.New("file upload exceeds its storage quota or size limit")
		}
		if err == nil {
			err = temporary.Chmod(0o660)
		}
		if err == nil {
			uid, gid, identityErr := taskIdentityIDs()
			if identityErr != nil {
				err = identityErr
			} else {
				err = temporary.Chown(uid, gid)
			}
		}
		if closeErr := temporary.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		temporaryLeaf := filepath.Base(temporaryName)
		if mode == "create" {
			return unix.Linkat(parentFD, temporaryLeaf, parentFD, leaf, 0)
		}
		return unix.Renameat(parentFD, temporaryLeaf, parentFD, leaf)
	case "file-mkdir":
		root, relative, err := fileArguments(arguments, 4)
		if err != nil || relative == "" {
			return errors.New("file-mkdir requires a directory path")
		}
		parentFD, leaf, err := openSharedParent(root, relative, true)
		if err != nil {
			return err
		}
		defer unix.Close(parentFD)
		if err := unix.Mkdirat(parentFD, leaf, 0o770); err != nil {
			return err
		}
		uid, gid, err := taskIdentityIDs()
		if err != nil {
			return err
		}
		return unix.Fchownat(parentFD, leaf, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
	case "file-rename":
		if len(arguments) != 5 || !safeMountPath(arguments[2]) {
			return errors.New("file-rename requires source and destination paths")
		}
		oldRelative, err := decodeRelativePath(arguments[3])
		if err != nil || oldRelative == "" {
			return errors.New("invalid source path")
		}
		newRelative, err := decodeRelativePath(arguments[4])
		if err != nil || newRelative == "" {
			return errors.New("invalid destination path")
		}
		oldParent, oldLeaf, err := openSharedParent(arguments[2], oldRelative, false)
		if err != nil {
			return err
		}
		defer unix.Close(oldParent)
		var sourceInfo unix.Stat_t
		if err := unix.Fstatat(oldParent, oldLeaf, &sourceInfo, unix.AT_SYMLINK_NOFOLLOW); err != nil || sourceInfo.Mode&unix.S_IFMT == unix.S_IFLNK {
			return errors.New("source path is unavailable or symbolic")
		}
		newParent, newLeaf, err := openSharedParent(arguments[2], newRelative, true)
		if err != nil {
			return err
		}
		defer unix.Close(newParent)
		return unix.Renameat2(oldParent, oldLeaf, newParent, newLeaf, unix.RENAME_NOREPLACE)
	case "file-delete":
		if len(arguments) < 5 || len(arguments) > 6 {
			return errors.New("file-delete requires a path and delete mode")
		}
		root, relative, err := fileArguments(arguments[:4], 4)
		mode := arguments[4]
		if err != nil || relative == "" || (mode != "single" && mode != "recursive" && mode != "owned-recursive") ||
			(mode == "owned-recursive") != (len(arguments) == 6) {
			return errors.New("file-delete requires a valid path and delete mode")
		}
		parentFD, leaf, err := openSharedParent(root, relative, false)
		if err != nil {
			return err
		}
		defer unix.Close(parentFD)
		var targetInfo unix.Stat_t
		if err := unix.Fstatat(parentFD, leaf, &targetInfo, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if mode == "single" {
			flags := 0
			if targetInfo.Mode&unix.S_IFMT == unix.S_IFDIR {
				flags = unix.AT_REMOVEDIR
			}
			return unix.Unlinkat(parentFD, leaf, flags)
		}
		path := filepath.Join(procDirectoryPath(parentFD), leaf)
		if mode == "owned-recursive" {
			return removeOwnedTreeAt(parentFD, leaf, relative, arguments[5])
		}
		return os.RemoveAll(path)
	default:
		return errors.New("unsupported mount-helper action")
	}
}

const sharedPathPolicyDirectory = "/etc/xirang/shared-write-paths.d"

var sharedPathPolicyServices = []string{"xirangd.service", "xirangd-standby.service", "xirang-agent.service"}

func updateSystemdSharedPathPolicy(id, path string, authorize bool) error {
	if err := writeSystemdSharedPathPolicy(sharedPathPolicyDirectory, "/etc/systemd/system", id, path, authorize); err != nil {
		return err
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		return runMountCommand("systemctl", "daemon-reload")
	}
	return nil
}

func writeSystemdSharedPathPolicy(policyDirectory, systemdDirectory, id, path string, authorize bool) error {
	if !safeMountID(id) || !safeSharedPolicyPath(path) {
		return errors.New("invalid shared path policy")
	}
	if err := os.MkdirAll(policyDirectory, 0o750); err != nil {
		return err
	}
	policyFile := filepath.Join(policyDirectory, id+".path")
	if authorize {
		cleaned := filepath.Clean(path)
		if err := os.WriteFile(policyFile, []byte(cleaned+"\n"), 0o640); err != nil {
			return err
		}
	} else if err := os.Remove(policyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(policyDirectory)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".path" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(policyDirectory, entry.Name()))
		candidate := filepath.Clean(strings.TrimSpace(string(raw)))
		if readErr != nil || !safeSharedPolicyPath(candidate) {
			return errors.New("shared path policy contains an invalid path")
		}
		if _, exists := seen[candidate]; !exists {
			seen[candidate] = struct{}{}
			paths = append(paths, candidate)
		}
	}
	sort.Strings(paths)
	for _, service := range sharedPathPolicyServices {
		dropInDirectory := filepath.Join(systemdDirectory, service+".d")
		dropIn := filepath.Join(dropInDirectory, "30-xirang-shared-paths.conf")
		if len(paths) == 0 {
			if err := os.Remove(dropIn); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(dropInDirectory, 0o755); err != nil {
			return err
		}
		content := "[Service]\nReadWritePaths=" + strings.Join(paths, " ") + "\n"
		if err := os.WriteFile(dropIn, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func safeSharedPolicyPath(value string) bool {
	if !safeMountPath(value) {
		return false
	}
	cleaned := filepath.Clean(value)
	for _, root := range []string{"/srv", "/shared", "/mnt", "/var/lib/xirang", "/var/lib/xirang-agent"} {
		if cleaned == root || strings.HasPrefix(cleaned, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func removeOwnedTree(target, targetRelative, encodedManifest string) error {
	parent, err := unix.Open(filepath.Dir(target), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return removeOwnedTreeAt(parent, filepath.Base(target), targetRelative, encodedManifest)
}

func removeOwnedTreeAt(parent int, name, targetRelative, encodedManifest string) error {
	targetRelative, err := cleanSharedRelativePath(targetRelative)
	if err != nil || targetRelative == "" {
		return errors.New("owned delete target is invalid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encodedManifest)
	if err != nil || len(raw) == 0 || len(raw) > 1<<20 {
		return errors.New("owned delete manifest is invalid")
	}
	var declared []string
	if err := json.Unmarshal(raw, &declared); err != nil || len(declared) == 0 || len(declared) > 100_000 {
		return errors.New("owned delete manifest is invalid")
	}
	allowed := make(map[string]struct{}, len(declared))
	for _, relative := range declared {
		cleaned, err := cleanSharedRelativePath(relative)
		if err != nil || cleaned == "" {
			return errors.New("owned delete manifest contains an invalid path")
		}
		allowed[cleaned] = struct{}{}
	}
	entry, err := inspectOwnedDelete(parent, name, targetRelative, allowed)
	if err != nil {
		return err
	}
	return removeOwnedEntry(parent, entry)
}

type ownedDeleteEntry struct {
	name     string
	stat     unix.Stat_t
	children []*ownedDeleteEntry
}

func sameOwnedEntry(left, right *unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Mode&unix.S_IFMT == right.Mode&unix.S_IFMT
}

// Validate the complete manifest before deleting anything. Only single path
// components are resolved relative to pinned directories, never a mutable
// multi-component path from an earlier walk.
func inspectOwnedDelete(parent int, name, relative string, allowed map[string]struct{}) (*ownedDeleteEntry, error) {
	if _, ok := allowed[relative]; !ok {
		return nil, fmt.Errorf("owned delete refused untracked path %q", relative)
	}
	entry := &ownedDeleteEntry{name: name}
	if err := unix.Fstatat(parent, name, &entry.stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if entry.stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return entry, nil
	}
	fd, err := openSharedAt(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), name)
	defer directory.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, err
	}
	if !sameOwnedEntry(&entry.stat, &opened) {
		return nil, errors.New("owned directory changed during validation")
	}
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	for _, child := range names {
		item, err := inspectOwnedDelete(fd, child, relative+"/"+child, allowed)
		if err != nil {
			return nil, err
		}
		entry.children = append(entry.children, item)
	}
	return entry, nil
}

func removeOwnedEntry(parent int, entry *ownedDeleteEntry) error {
	var current unix.Stat_t
	if err := unix.Fstatat(parent, entry.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameOwnedEntry(&entry.stat, &current) {
		return errors.New("owned entry changed before deletion")
	}
	if current.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(parent, entry.name, 0)
	}
	fd, err := openSharedAt(parent, entry.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if !sameOwnedEntry(&entry.stat, &opened) {
		return errors.New("owned directory changed before deletion")
	}
	for _, child := range entry.children {
		if err := removeOwnedEntry(fd, child); err != nil {
			return err
		}
	}
	// Any late arrival makes this fail with ENOTEMPTY instead of being removed.
	return unix.Unlinkat(parent, entry.name, unix.AT_REMOVEDIR)
}

func cleanSharedRelativePath(value string) (string, error) {
	if len(value) > 1024 || strings.ContainsRune(value, 0) {
		return "", errors.New("invalid relative path")
	}
	relative := filepath.ToSlash(filepath.Clean(value))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", errors.New("relative path escapes shared space")
	}
	return relative, nil
}

// directoryUsedBytes reports the apparent size of regular files below root.
// Filesystem capacity remains available from Statfs, but it must not be
// presented as the amount of data stored in one shared directory.
func directoryUsedBytes(root string) (uint64, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("directory size requires filesystem metadata")
	}
	var used uint64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		stat, statOK := info.Sys().(*syscall.Stat_t)
		if statOK && stat.Dev != rootStat.Dev {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return nil
		}
		size := uint64(info.Size())
		if ^uint64(0)-used < size {
			return errors.New("directory size exceeds uint64")
		}
		used += size
		return nil
	})
	return used, err
}

type sharedFileEntry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Directory  bool      `json:"directory"`
	SizeBytes  uint64    `json:"sizeBytes"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Owner      string    `json:"owner"`
}

func fileArguments(arguments []string, length int) (string, string, error) {
	if len(arguments) != length || !safeMountPath(arguments[2]) {
		return "", "", errors.New("file action requires a safe shared-space path")
	}
	relative, err := decodeRelativePath(arguments[3])
	if err != nil {
		return "", "", err
	}
	return arguments[2], relative, nil
}

func decodeRelativePath(value string) (string, error) {
	if value == "_" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 1024 {
		return "", errors.New("invalid encoded relative path")
	}
	relative := filepath.ToSlash(filepath.Clean(string(decoded)))
	if relative == "." {
		return "", nil
	}
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsRune(relative, 0) {
		return "", errors.New("relative path escapes shared space")
	}
	return relative, nil
}

func secureSharedPath(root, relative string, allowMissing bool) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(resolvedRoot, filepath.FromSlash(relative))
	check := candidate
	if allowMissing {
		for {
			if _, statErr := os.Lstat(check); statErr == nil {
				break
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return "", statErr
			}
			parent := filepath.Dir(check)
			if parent == check {
				return "", errors.New("shared path has no existing parent")
			}
			check = parent
		}
	}
	resolved, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", err
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(os.PathSeparator)) {
		return "", errors.New("shared path escapes shared space")
	}
	if !allowMissing {
		return resolved, nil
	}
	suffix, err := filepath.Rel(check, candidate)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, suffix), nil
}

const sharedPathResolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS

// openSharedParent pins every existing directory with a file descriptor and
// returns the descriptor for the final parent plus the last path component.
// Mutations performed relative to this descriptor cannot be redirected by
// replacing an already validated parent with a symlink.
func openSharedParent(root, relative string, createParents bool) (int, string, error) {
	cleaned, err := cleanSharedRelativePath(relative)
	if err != nil || cleaned == "" {
		return -1, "", errors.New("shared file path is invalid")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return -1, "", err
	}
	current, err := unix.Open(resolvedRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", err
	}
	parts := strings.Split(filepath.FromSlash(cleaned), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, openErr := openSharedAt(current, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC)
		if errors.Is(openErr, unix.ENOENT) && createParents {
			if mkdirErr := unix.Mkdirat(current, part, 0o770); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				unix.Close(current)
				return -1, "", mkdirErr
			}
			uid, gid, identityErr := taskIdentityIDs()
			if identityErr != nil {
				unix.Close(current)
				return -1, "", identityErr
			}
			if chownErr := unix.Fchownat(current, part, uid, gid, unix.AT_SYMLINK_NOFOLLOW); chownErr != nil {
				unix.Close(current)
				return -1, "", chownErr
			}
			next, openErr = openSharedAt(current, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC)
		}
		if openErr != nil {
			unix.Close(current)
			return -1, "", openErr
		}
		unix.Close(current)
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func openSharedDirectory(root, relative string) (*os.File, error) {
	if relative == "" {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		fd, err := unix.Open(resolvedRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), resolvedRoot), nil
	}
	parent, leaf, err := openSharedParent(root, relative, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	fd, err := openSharedAt(parent, leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), relative), nil
}

func openSharedRegularFile(root, relative string) (*os.File, error) {
	parent, leaf, err := openSharedParent(root, relative, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	fd, err := openSharedAt(parent, leaf, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), relative)
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("file-read requires a regular file")
	}
	return file, nil
}

func procDirectoryPath(fd int) string {
	return filepath.Join("/proc/self/fd", strconv.Itoa(fd))
}

func openSharedAt(parent int, name string, flags int) (int, error) {
	fd, err := unix.Openat2(parent, name, &unix.OpenHow{Flags: uint64(flags), Resolve: sharedPathResolveFlags})
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
		return fd, err
	}
	// Every caller supplies exactly one validated path component, so the
	// descriptor-relative O_NOFOLLOW fallback preserves the same no-symlink
	// property on kernels predating openat2.
	return unix.Openat(parent, name, flags|unix.O_NOFOLLOW, 0)
}

func taskIdentityIDs() (int, int, error) {
	taskIdentity, err := user.Lookup("xirang-task")
	if err != nil {
		return 0, 0, errors.New("xirang-task identity is required")
	}
	uid, uidErr := strconv.Atoi(taskIdentity.Uid)
	gid, gidErr := strconv.Atoi(taskIdentity.Gid)
	if uidErr != nil || gidErr != nil {
		return 0, 0, errors.New("xirang-task identity has invalid numeric IDs")
	}
	return uid, gid, nil
}

func fileOwner(info os.FileInfo) string {
	statistics, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	identity, err := user.LookupId(strconv.FormatUint(uint64(statistics.Uid), 10))
	if err == nil {
		return identity.Username
	}
	return strconv.FormatUint(uint64(statistics.Uid), 10)
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

func putFSTab(id, source, target string) error {
	if err := removeFSTab(id); err != nil {
		return err
	}
	file, err := os.OpenFile("/etc/fstab", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%s %s nfs rw,hard,timeo=60,retrans=2,_netdev,nofail 0 0 # xirang:%s\n", source, target, id)
	return err
}

func removeFSTab(id string) error {
	encoded, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	marker := "# xirang:" + id
	lines := []string{}
	scanner := bufio.NewScanner(strings.NewReader(string(encoded)))
	for scanner.Scan() {
		if !strings.Contains(scanner.Text(), marker) {
			lines = append(lines, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return os.WriteFile("/etc/fstab", []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func runMountCommand(name string, arguments ...string) error {
	output, err := exec.Command(name, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, strings.TrimSpace(string(output)))
	}
	return nil
}

func safeMountID(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func safeMountHost(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune(".:-", character)) {
			return false
		}
	}
	return true
}

func safeMountPath(value string) bool {
	if !filepath.IsAbs(value) || value == "/" || len(value) > 512 {
		return false
	}
	// The character set permits dots, so a traversal has to be rejected
	// explicitly. The controller cleans these paths before sending them, but
	// this helper is the last check standing between a malformed request and
	// root creating, chowning, exporting or unmounting the directory it names.
	if value != filepath.Clean(value) {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("/._-", character)) {
			return false
		}
	}
	return true
}
