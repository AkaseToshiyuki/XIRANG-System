package agent

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux UAPI: linux/fs.h, linux/quota.h and linux/dqblk_xfs.h. These structures
// and ioctl encodings are shared by the supported Linux amd64/arm64 targets.
const (
	fsGetXAttr       = 0x801c581f
	fsSetXAttr       = 0x401c5820
	projectInherit   = 0x00000200
	getProjectQuota  = (0x800007 << 8) | 2
	setProjectQuota  = (0x800008 << 8) | 2
	getQuotaState    = (((int('X') << 8) + 5) << 8) | 2
	projectAccounted = 1 << 4
	projectEnforced  = 1 << 5
	loopBlockMajor   = 7 // linux/major.h, LOOP_MAJOR
	quotaRegistry    = "/var/lib/xirang-shared-quotas"
)

type projectAttributes struct {
	Flags, ExtentSize, Extents, ProjectID, CowExtentSize uint32
	Padding                                              [8]byte
}

type projectQuota struct {
	Hard, Soft, Used, InodeHard, InodeSoft, Inodes, BlockTime, InodeTime uint64
	Valid                                                                uint32
	Padding                                                              [4]byte
}

type sharedQuotaObservation struct {
	Status         string `json:"quotaStatus"`
	LimitBytes     uint64 `json:"quotaEnforcedBytes"`
	AllocatedBytes uint64 `json:"quotaAllocatedBytes"`
	Error          string `json:"quotaError,omitempty"`
}

type sharedQuotaRecord struct {
	Path         string `json:"path"`
	Device       uint64 `json:"device"`
	FilesystemID string `json:"filesystemId,omitempty"`
	Inode        uint64 `json:"inode"`
	ProjectID    uint32 `json:"projectId"`
	Complete     bool   `json:"complete"`
}

func projectAttributesAt(fd int, value *projectAttributes, set bool) error {
	command := uintptr(fsGetXAttr)
	if set {
		command = fsSetXAttr
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), command, uintptr(unsafe.Pointer(value)))
	runtime.KeepAlive(value)
	if errno != 0 {
		return errno
	}
	return nil
}

func projectQuotaCall(fd, command int, project uint32, value unsafe.Pointer) error {
	_, _, errno := unix.Syscall6(unix.SYS_QUOTACTL_FD, uintptr(fd), uintptr(command), uintptr(project), uintptr(value), 0, 0)
	runtime.KeepAlive(value)
	if errno != 0 {
		return errno
	}
	return nil
}

func readProjectQuota(fd int, project uint32) (projectQuota, error) {
	var value projectQuota
	err := projectQuotaCall(fd, getProjectQuota, project, unsafe.Pointer(&value))
	// XFS returns ENOENT for a project which has never been allocated.
	if errors.Is(err, unix.ENOENT) {
		err = nil
	}
	return value, err
}

func projectQuotaAvailable(fd int) error {
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return err
	}
	if filesystem.Type != unix.EXT4_SUPER_MAGIC && filesystem.Type != unix.XFS_SUPER_MAGIC {
		return errors.New("shared capacity limits require ext4 or XFS project quotas")
	}
	// Q_XGETQSTAT uses the generic quotactl get_state adapter on both filesystems.
	// qs_flags is a native uint16 at offset 2. Reserve more than the UAPI struct
	// size; only the accounting/enforcement bits are consumed here.
	var state [256]byte
	if err := projectQuotaCall(fd, getQuotaState, 0, unsafe.Pointer(&state[0])); err != nil {
		return fmt.Errorf("cannot inspect project quota enforcement: %w", err)
	}
	flags := binary.NativeEndian.Uint16(state[2:4])
	if flags&(projectAccounted|projectEnforced) != projectAccounted|projectEnforced {
		return errors.New("project quota enforcement is not enabled on this filesystem")
	}
	return nil
}

func readSharedQuotaRecord(registry, id string) (sharedQuotaRecord, error) {
	var record sharedQuotaRecord
	fd, err := unix.Open(filepath.Join(registry, id+".json"), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return record, err
	}
	file := os.NewFile(uintptr(fd), "quota record")
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return record, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != 0 || stat.Mode&0o022 != 0 || stat.Size > 4096 {
		return record, errors.New("unsafe shared quota record")
	}
	if err := json.NewDecoder(file).Decode(&record); err != nil {
		return record, err
	}
	if record.ProjectID == 0 || !safeMountPath(record.Path) {
		return record, errors.New("invalid shared quota record")
	}
	return record, nil
}

func quotaFilesystemID(fd int) (string, error) {
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(fd, &filesystem); err != nil {
		return "", err
	}
	// ext4_statfs derives f_fsid from its on-disk UUID. XFS instead derives
	// f_fsid from the device number, so retain the existing device contract there.
	if filesystem.Type != unix.EXT4_SUPER_MAGIC {
		return "", nil
	}
	return fmt.Sprintf("ext4:%08x%08x", uint32(filesystem.Fsid.Val[0]), uint32(filesystem.Fsid.Val[1])), nil
}

func recordMatchesRoot(record sharedQuotaRecord, path string, stat unix.Stat_t, filesystemID string) bool {
	deviceMatches := record.Device == uint64(stat.Dev)
	if record.FilesystemID != "" {
		deviceMatches = record.FilesystemID == filesystemID
	}
	return record.Path == path && deviceMatches && record.Inode == stat.Ino
}

func inspectSharedQuota(id, path string) sharedQuotaObservation {
	observation := sharedQuotaObservation{Status: "unknown"}
	directory, err := openSharedDirectory(path, "")
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	defer directory.Close()
	fd := int(directory.Fd())
	if err := projectQuotaAvailable(fd); err != nil {
		observation.Status, observation.Error = "unsupported", err.Error()
		return observation
	}
	var attributes projectAttributes
	if err := projectAttributesAt(fd, &attributes, false); err != nil {
		observation.Error = err.Error()
		return observation
	}
	record, err := readSharedQuotaRecord(quotaRegistry, id)
	if errors.Is(err, os.ErrNotExist) && attributes.ProjectID == 0 {
		observation.Status = "available"
		return observation
	}
	var stat unix.Stat_t
	filesystemID, identityErr := quotaFilesystemID(fd)
	if err != nil || identityErr != nil || !record.Complete || unix.Fstat(fd, &stat) != nil || !recordMatchesRoot(record, path, stat, filesystemID) || attributes.ProjectID != record.ProjectID || attributes.Flags&projectInherit == 0 {
		observation.Error = "directory project is unmanaged, incomplete, or no longer matches its quota record"
		return observation
	}
	quota, err := readProjectQuota(fd, record.ProjectID)
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	observation.Status, observation.LimitBytes, observation.AllocatedBytes = "unlimited", quota.Hard*1024, quota.Used
	if quota.Hard != 0 {
		observation.Status = "enforced"
	}
	return observation
}

// sharedFilesystemStatistics reads the filesystem's root, not the project
// directory: project-aware statfs may otherwise report the quota as disk size.
// File-backed loop filesystems are also bounded by their backing filesystem's
// currently available space; sparse image size is not physical free capacity.
func sharedFilesystemStatistics(path string) (unix.Statfs_t, error) {
	var result unix.Statfs_t
	directory, err := openSharedDirectory(path, "")
	if err != nil {
		return result, err
	}
	defer directory.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return result, err
	}
	contents, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return result, err
	}
	read := func(device uint64) (unix.Statfs_t, error) {
		return filesystemStatisticsForDevice(string(contents), device)
	}
	return boundedFilesystemStatistics(uint64(stat.Dev), read, loopBackingDevice)
}

func filesystemStatisticsForDevice(contents string, device uint64) (unix.Statfs_t, error) {
	var result unix.Statfs_t
	mount, err := filesystemRootMount(contents, device)
	if err != nil {
		return result, err
	}
	root, err := os.Open(mount)
	if err != nil {
		return result, err
	}
	defer root.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil || uint64(rootStat.Dev) != device {
		return result, errors.New("filesystem mount changed while reading capacity")
	}
	err = unix.Fstatfs(int(root.Fd()), &result)
	return result, err
}

func loopBackingDevice(device uint64) (uint64, bool, error) {
	if unix.Major(device) != loopBlockMajor {
		return 0, false, nil
	}
	path := fmt.Sprintf("/dev/loop%d", unix.Minor(device))
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, true, fmt.Errorf("cannot inspect loop backing storage: %w", err)
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFBLK || uint64(stat.Rdev) != device {
		return 0, true, errors.New("loop device changed during capacity inspection")
	}
	info, err := unix.IoctlLoopGetStatus64(fd)
	if err != nil {
		return 0, true, fmt.Errorf("cannot read loop backing storage: %w", err)
	}
	// A loop over a raw block device has no backing-file free-space contract.
	// Do not substitute the devtmpfs free space for its physical capacity.
	if info.Rdevice != 0 || info.Device == 0 || info.Number != unix.Minor(device) {
		return 0, true, errors.New("loop capacity requires a regular-file backing filesystem")
	}
	return info.Device, true, nil
}

func boundedFilesystemStatistics(device uint64, read func(uint64) (unix.Statfs_t, error), backing func(uint64) (uint64, bool, error)) (unix.Statfs_t, error) {
	var result unix.Statfs_t
	seen := map[uint64]bool{}
	for depth := 0; depth < 8; depth++ {
		if seen[device] {
			return unix.Statfs_t{}, errors.New("cyclic loop backing storage")
		}
		seen[device] = true
		current, err := read(device)
		if err != nil {
			return unix.Statfs_t{}, err
		}
		if current.Bsize <= 0 || current.Blocks > ^uint64(0)/uint64(current.Bsize) || current.Bavail > current.Blocks || current.Bfree > current.Blocks {
			return unix.Statfs_t{}, errors.New("invalid filesystem capacity")
		}
		if depth == 0 {
			result = current
		} else {
			unit := uint64(current.Bsize)
			result.Blocks = min(result.Blocks, current.Blocks*unit/uint64(result.Bsize))
			result.Bfree = min(result.Bfree, current.Bfree*unit/uint64(result.Bsize))
			result.Bavail = min(result.Bavail, current.Bavail*unit/uint64(result.Bsize))
		}
		next, exists, err := backing(device)
		if err != nil {
			return unix.Statfs_t{}, err
		}
		if !exists {
			return result, nil
		}
		device = next
	}
	return unix.Statfs_t{}, errors.New("loop backing storage depth exceeds capacity inspection limit")
}

func filesystemRootMount(contents string, device uint64) (string, error) {
	deviceID := fmt.Sprintf("%d:%d", unix.Major(device), unix.Minor(device))
	scanner := bufio.NewScanner(strings.NewReader(contents))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 || fields[2] != deviceID || fields[3] != "/" {
			continue
		}
		return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4]), nil
	}
	return "", errors.New("underlying filesystem root is unavailable for disk capacity measurement")
}

func applySharedQuota(id, path string, limit uint64) error {
	if !safeMountID(id) || !safeSharedPolicyPath(path) || limit > 1<<60 || limit != 0 && (limit < 1024 || limit%1024 != 0) {
		return errors.New("capacity limit must be zero or a positive whole number of KiB")
	}
	directory, err := openSharedDirectory(path, "")
	if err != nil {
		return err
	}
	defer directory.Close()
	fd := int(directory.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	filesystemID, err := quotaFilesystemID(fd)
	if err != nil {
		return err
	}
	// This separate lock also serializes local administrative helper calls.
	registry, err := openOrCreateSharedDirectory(quotaRegistry)
	if err != nil {
		return err
	}
	defer registry.Close()
	if err := unix.Flock(int(registry.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("another shared quota update is running")
	}
	defer unix.Flock(int(registry.Fd()), unix.LOCK_UN)
	var registryStat unix.Stat_t
	if err := unix.Fstat(int(registry.Fd()), &registryStat); err != nil || registryStat.Uid != 0 || registryStat.Mode&0o022 != 0 {
		return errors.New("quota registry must be root-owned and protected")
	}
	record, recordErr := readSharedQuotaRecord(quotaRegistry, id)
	if recordErr != nil && !errors.Is(recordErr, os.ErrNotExist) {
		return recordErr
	}
	if recordErr == nil && !recordMatchesRoot(record, path, stat, filesystemID) {
		return errors.New("shared directory was replaced; quota record no longer matches")
	}
	if err := projectQuotaAvailable(fd); err != nil {
		// Zero is the default on an unconfigured filesystem, not a claim that
		// a previously installed limit has been removed successfully.
		if limit == 0 && errors.Is(recordErr, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if limit != 0 {
		statistics, err := sharedFilesystemStatistics(path)
		if err != nil {
			return err
		}
		if limit > statistics.Bavail*uint64(statistics.Bsize) {
			return errors.New("capacity limit exceeds the disk's current available space")
		}
	}
	var attributes projectAttributes
	if err := projectAttributesAt(fd, &attributes, false); err != nil {
		return err
	}
	if errors.Is(recordErr, os.ErrNotExist) {
		if attributes.ProjectID != 0 {
			return errors.New("directory already belongs to an unmanaged project")
		}
		if limit == 0 {
			return nil
		}
		record = sharedQuotaRecord{Path: path, Device: uint64(stat.Dev), FilesystemID: filesystemID, Inode: stat.Ino}
		if err := reserveSharedProject(fd, id, &record); err != nil {
			return err
		}
	}
	// Validate before touching any inode. A deadline/partial tagging error
	// retains the protected record for an idempotent retry, without installing
	// a new hard limit or claiming that the whole tree is enforced.
	if !record.Complete {
		deadline := time.Now().Add(20 * time.Second)
		for mode := 0; mode < 3; mode++ { // inspect, assign, independently read back
			count := 0
			if err := tagSharedProject(directory, stat.Dev, record.ProjectID, mode, deadline, &count, 0); err != nil {
				return err
			}
		}
		record.Complete = true
		encoded, _ := json.Marshal(record)
		if err := writeMountConfiguration(filepath.Join(quotaRegistry, id+".json"), encoded); err != nil {
			return err
		}
	} else if attributes.ProjectID != record.ProjectID || attributes.Flags&projectInherit == 0 {
		return errors.New("shared directory no longer inherits its managed project quota")
	}
	current, err := readProjectQuota(fd, record.ProjectID)
	if err != nil {
		return err
	}
	if limit != 0 && current.Used > limit {
		return errors.New("capacity limit is below the directory's allocated space")
	}
	value := projectQuota{Hard: limit / 1024, Valid: 1} // QIF_BLIMITS, zero soft limit
	if err := projectQuotaCall(fd, setProjectQuota, record.ProjectID, unsafe.Pointer(&value)); err != nil {
		return err
	}
	actual, err := readProjectQuota(fd, record.ProjectID)
	if err != nil || actual.Hard != limit/1024 || actual.Soft != 0 {
		return errors.New("kernel quota readback did not confirm the requested capacity limit")
	}
	return nil
}

func reserveSharedProject(fd int, id string, record *sharedQuotaRecord) error {
	entries, err := os.ReadDir(quotaRegistry)
	if err != nil {
		return err
	}
	used := map[uint32]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		other, err := readSharedQuotaRecord(quotaRegistry, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return err
		}
		used[other.ProjectID] = true
		sameFilesystem := other.Device == record.Device
		if other.FilesystemID != "" && record.FilesystemID != "" {
			sameFilesystem = other.FilesystemID == record.FilesystemID
		}
		if sameFilesystem && (other.Inode == record.Inode || strings.HasPrefix(record.Path+"/", other.Path+"/") || strings.HasPrefix(other.Path+"/", record.Path+"/")) {
			return errors.New("overlapping managed project quota directories are not supported")
		}
	}
	for attempt := 0; attempt < 32; attempt++ {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		candidate := binary.LittleEndian.Uint32(random[:])&0x3fffffff | 0x40000000
		if used[candidate] {
			continue
		}
		quota, err := readProjectQuota(fd, candidate)
		if err != nil {
			return err
		}
		if quota.Hard != 0 || quota.Soft != 0 || quota.InodeHard != 0 || quota.InodeSoft != 0 || quota.Used != 0 || quota.Inodes != 0 {
			continue
		}
		record.ProjectID = candidate
		encoded, _ := json.Marshal(record)
		file, err := os.OpenFile(filepath.Join(quotaRegistry, id+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(encoded)
		syncErr, closeErr := file.Sync(), file.Close()
		return errors.Join(writeErr, syncErr, closeErr)
	}
	return errors.New("could not reserve an unused project quota ID")
}

func tagSharedProject(file *os.File, device uint64, project uint32, mode int, deadline time.Time, count *int, depth int) error {
	*count++
	if *count > 100000 || depth > 128 || time.Now().After(deadline) {
		return errors.New("shared quota initialization exceeded its bounded tree scan; retry after reducing the tree")
	}
	fd := int(file.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	isDirectory := stat.Mode&unix.S_IFMT == unix.S_IFDIR
	if stat.Dev != device || !isDirectory && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1) {
		return errors.New("quota initialization requires regular files and directories without hard links or nested filesystems")
	}
	var attributes projectAttributes
	if err := projectAttributesAt(fd, &attributes, false); err != nil {
		return err
	}
	if attributes.ProjectID != 0 && attributes.ProjectID != project {
		return errors.New("shared directory contains an inode assigned to another project")
	}
	if mode == 2 && (attributes.ProjectID != project || isDirectory && attributes.Flags&projectInherit == 0) {
		return errors.New("directory changed during project quota initialization; retry the update")
	}
	if mode == 1 && (attributes.ProjectID != project || isDirectory && attributes.Flags&projectInherit == 0) {
		attributes.ProjectID = project
		if isDirectory {
			attributes.Flags |= projectInherit
		}
		if err := projectAttributesAt(fd, &attributes, true); err != nil {
			return err
		}
	}
	if !isDirectory {
		return nil
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	for {
		entries, err := file.ReadDir(128)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			// Pin with O_PATH before opening. Reject special files without ever
			// opening a device/FIFO, even if a writer swaps the name concurrently.
			pinned, err := unix.Openat(fd, entry.Name(), unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			var childStat unix.Stat_t
			err = unix.Fstat(pinned, &childStat)
			if err != nil || childStat.Dev != device || (childStat.Mode&unix.S_IFMT != unix.S_IFREG && childStat.Mode&unix.S_IFMT != unix.S_IFDIR) {
				unix.Close(pinned)
				return errors.New("quota initialization does not support symlinks, special files, or nested filesystems")
			}
			child, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", pinned))
			unix.Close(pinned)
			if err != nil {
				return err
			}
			err = tagSharedProject(child, device, project, mode, deadline, count, depth+1)
			child.Close()
			if err != nil {
				return err
			}
		}
	}
}
