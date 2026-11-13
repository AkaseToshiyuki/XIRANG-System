package agent

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestProjectQuotaABIWithIndependentLinuxHeaders(t *testing.T) {
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable for independent Linux UAPI check")
	}
	directory := t.TempDir()
	source := filepath.Join(directory, "abi.c")
	program := `#include <stdio.h>
#include <stddef.h>
#include <sys/ioctl.h>
#include <linux/fs.h>
#include <linux/quota.h>
#include <linux/dqblk_xfs.h>
#include <linux/major.h>
int main(void) {
 printf("{\"attributes\":%zu,\"quota\":%zu,\"getattr\":%lu,\"setattr\":%lu,\"inherit\":%u,\"getquota\":%u,\"setquota\":%u,\"state\":%u,\"stateFlagsOffset\":%zu,\"enforcement\":%u,\"loopMajor\":%u}",
 sizeof(struct fsxattr), sizeof(struct if_dqblk), (unsigned long)FS_IOC_FSGETXATTR, (unsigned long)FS_IOC_FSSETXATTR, FS_XFLAG_PROJINHERIT,
 (unsigned)QCMD(Q_GETQUOTA, PRJQUOTA), (unsigned)QCMD(Q_SETQUOTA, PRJQUOTA), (unsigned)QCMD(Q_XGETQSTAT, PRJQUOTA), offsetof(fs_quota_stat_t, qs_flags), FS_QUOTA_PDQ_ACCT | FS_QUOTA_PDQ_ENFD, LOOP_MAJOR);
}`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "abi")
	if output, err := exec.Command(compiler, source, "-o", binary).CombinedOutput(); err != nil {
		t.Fatalf("compile Linux headers: %s %v", output, err)
	}
	output, err := exec.Command(binary).Output()
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]uint64
	if err := json.Unmarshal(output, &values); err != nil {
		t.Fatal(err)
	}
	expected := map[string]uint64{"attributes": uint64(unsafe.Sizeof(projectAttributes{})), "quota": uint64(unsafe.Sizeof(projectQuota{})), "getattr": fsGetXAttr, "setattr": fsSetXAttr, "inherit": projectInherit, "getquota": getProjectQuota, "setquota": setProjectQuota, "state": uint64(getQuotaState), "stateFlagsOffset": 2, "enforcement": projectAccounted | projectEnforced}
	expected["loopMajor"] = loopBlockMajor
	for name, want := range expected {
		if values[name] != want {
			t.Errorf("Linux %s=%d; Go value=%d", name, values[name], want)
		}
	}
}

func TestSharedCapacityIncludesSparseBackingAndDifferentBlockSizes(t *testing.T) {
	// A 512 MiB image is hosted on a 256 MiB filesystem, which is itself
	// file-backed on a disk with only 48 MiB available. Inner free space alone
	// would incorrectly allow a 100 MiB quota. Use independent byte quantities.
	stats := map[uint64]unix.Statfs_t{
		1: {Bsize: 4096, Blocks: 131072, Bfree: 120000, Bavail: 110000},
		2: {Bsize: 1024, Blocks: 262144, Bfree: 90000, Bavail: 80000},
		3: {Bsize: 4096, Blocks: 524288, Bfree: 16384, Bavail: 12288},
	}
	read := func(device uint64) (unix.Statfs_t, error) { return stats[device], nil }
	backing := func(device uint64) (uint64, bool, error) { return device + 1, device < 3, nil }
	result, err := boundedFilesystemStatistics(1, read, backing)
	if err != nil || result.Bavail*uint64(result.Bsize) != 48<<20 || result.Blocks*uint64(result.Bsize) != 256<<20 {
		t.Fatalf("incorrect backing capacity: %+v %v", result, err)
	}
	stats[3] = unix.Statfs_t{Bsize: 4096, Blocks: 524288, Bfree: 200000, Bavail: 190000}
	result, err = boundedFilesystemStatistics(1, read, backing)
	if err != nil || result.Bavail*uint64(result.Bsize) != 80000*1024 {
		t.Fatalf("intermediate backing limit was ignored: %+v %v", result, err)
	}
}

func TestExt4QuotaIdentitySurvivesDeviceRenumberingButNotReplacement(t *testing.T) {
	record := sharedQuotaRecord{Path: "/srv/share", Device: unix.Mkdev(7, 20), Inode: 12, FilesystemID: "ext4:12345678abcdef00"}
	remounted := unix.Stat_t{Dev: unix.Mkdev(7, 21), Ino: 12}
	if !recordMatchesRoot(record, "/srv/share", remounted, record.FilesystemID) {
		t.Fatal("same ext4 filesystem was rejected after loop device renumbering")
	}
	if recordMatchesRoot(record, "/srv/share", remounted, "ext4:00000000abcdef00") || recordMatchesRoot(record, "/srv/other", remounted, record.FilesystemID) {
		t.Fatal("replaced filesystem or source path was accepted")
	}
	remounted.Ino++
	if recordMatchesRoot(record, "/srv/share", remounted, record.FilesystemID) {
		t.Fatal("replaced directory inode was accepted")
	}
	legacy := sharedQuotaRecord{Path: "/srv/share", Device: unix.Mkdev(8, 1), Inode: 12}
	if !recordMatchesRoot(legacy, "/srv/share", unix.Stat_t{Dev: legacy.Device, Ino: 12}, record.FilesystemID) || recordMatchesRoot(legacy, "/srv/share", remounted, record.FilesystemID) {
		t.Fatal("legacy quota record device boundary changed")
	}
}

func TestSharedCapacityFailsWhenBackingCannotBeEstablished(t *testing.T) {
	read := func(uint64) (unix.Statfs_t, error) {
		return unix.Statfs_t{Bsize: 4096, Blocks: 100, Bfree: 80, Bavail: 70}, nil
	}
	for name, backing := range map[string]func(uint64) (uint64, bool, error){
		"missing-device": func(uint64) (uint64, bool, error) { return 0, true, os.ErrNotExist },
		"cycle":          func(device uint64) (uint64, bool, error) { return device, true, nil },
		"depth":          func(device uint64) (uint64, bool, error) { return device + 1, true, nil },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := boundedFilesystemStatistics(1, read, backing); err == nil {
				t.Fatal("unsafe backing chain accepted")
			}
		})
	}
	missing := errors.New("backing filesystem is not visible")
	if _, err := boundedFilesystemStatistics(1, func(uint64) (unix.Statfs_t, error) { return unix.Statfs_t{}, missing }, loopBackingDevice); !errors.Is(err, missing) {
		t.Fatalf("backing inspection error hidden: %v", err)
	}
}

func TestSharedMountGuardAndExportOptionPreservation(t *testing.T) {
	for _, option := range []string{"mountpoint", "mp"} {
		for _, line := range []string{"/srv/share *(" + option + ",rw,all_squash)", "/srv/share *(rw,all_squash," + option + ")"} {
			if err := validateSharedExport(line, "/srv/share"); err != nil || !sharedExportRequiresMount(line) {
				t.Fatalf("mount guard not recognized: %q %v", line, err)
			}
		}
		if err := validateSharedExport("/srv/share *(rw,all_squash,"+option+"=/elsewhere)", "/srv/share"); err == nil {
			t.Fatal("alternate guard would be lost during managed export rewrite")
		}
	}
	if sharedExportRequiresMount("/srv/share *(rw,all_squash)") {
		t.Fatal("legacy directory export was made mount-only")
	}
	if err := requireSharedSourceMount(t.TempDir()); err == nil {
		t.Fatal("ordinary directory satisfied required source mount")
	}
	if err := requireSharedSourceMount("/"); err != nil {
		t.Fatalf("real mounted filesystem rejected: %v", err)
	}
}

func TestSharedQuotaReadsUnderlyingDiskInsteadOfBindRoot(t *testing.T) {
	device := unix.Mkdev(8, 1)
	fixture := "42 20 8:1 /project /srv/shared rw - ext4 /dev/sda1 rw,prjquota\n43 20 8:1 / /mnt/data\\040disk rw - ext4 /dev/sda1 rw,prjquota\n"
	path, err := filesystemRootMount(fixture, device)
	if err != nil || path != "/mnt/data disk" {
		t.Fatalf("mount=%q err=%v", path, err)
	}
	if _, err := filesystemRootMount(strings.Split(fixture, "\n")[0], device); err == nil {
		t.Fatal("a project bind root was treated as whole-disk capacity")
	}
}

func TestSharedFileHelperRequiresExactSquashedExport(t *testing.T) {
	valid := "/srv/share *(rw,sync,all_squash,anonuid=123,anongid=123)\n"
	if err := validateSharedExport(valid, "/srv/share"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ config, path string }{
		{valid, "/etc"}, {valid, "/srv/share/child"},
		{strings.ReplaceAll(valid, "all_squash", "no_root_squash"), "/srv/share"},
		{valid + "/etc *(rw)\n", "/srv/share"},
	} {
		if err := validateSharedExport(tc.config, tc.path); err == nil {
			t.Fatalf("accepted unregistered root: %+v", tc)
		}
	}
}

func TestQuotaTreeScanRejectsHardLinksWithoutChangingAttributes(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	if err := os.WriteFile(first, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	var before projectAttributes
	if err := projectAttributesAt(int(directory.Fd()), &before, false); err != nil {
		t.Skipf("filesystem cannot read project attributes: %v", err)
	}
	count := 0
	err = tagSharedProject(directory, stat.Dev, 42, 0, time.Now().Add(time.Second), &count, 0)
	if err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("hard links not rejected: %v", err)
	}
	var after projectAttributes
	if err := projectAttributesAt(int(directory.Fd()), &after, false); err != nil || before != after {
		t.Fatalf("preflight changed metadata: %v", err)
	}
}
