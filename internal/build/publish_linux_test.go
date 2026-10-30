package build

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPublishedBuildCrossIdentityAccess(t *testing.T) {
	if path := os.Getenv("XIRANG_PUBLICATION_TEST_PATH"); path != "" {
		data, err := os.ReadFile(path)
		if os.Getenv("XIRANG_PUBLICATION_TEST_DENIED") == "true" {
			if !os.IsPermission(err) {
				t.Fatalf("unrelated identity read output: %v", err)
			}
		} else if err != nil || string(data) != "published fixture" || unix.Access(path, unix.R_OK|unix.X_OK) != nil {
			t.Fatalf("task identity cannot read/execute published output: %v", err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("cross-identity publication access requires root")
	}
	root, err := os.MkdirTemp("", "xirang-publication-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	binary := filepath.Join(root, "build.test")
	output, err := os.OpenFile(binary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy helper: %v %v", copyErr, closeErr)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "input")
	if err := os.WriteFile(source, []byte("published fixture"), 0o500); err != nil {
		t.Fatal(err)
	}
	service := &Service{sharedWorkspace: true, sharedUID: 0, sharedGID: 400000012}
	destination := filepath.Join(root, "outputs", "nested", "app")
	if err := service.publishBuildOutput(source, destination); err != nil {
		t.Fatal(err)
	}
	for _, denied := range []bool{false, true} {
		group := uint32(400000012)
		if denied {
			group = 400000013
		}
		command := exec.Command(binary, "-test.run=^TestPublishedBuildCrossIdentityAccess$")
		command.Env = append(os.Environ(), "XIRANG_PUBLICATION_TEST_PATH="+destination)
		if denied {
			command.Env = append(command.Env, "XIRANG_PUBLICATION_TEST_DENIED=true")
		}
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 400000011, Gid: group}}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("cross-identity access denied=%t: %v %s", denied, err, output)
		}
	}
}

func TestPublicationPreservesExistingDirectoryAndSharesNewPaths(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "shared")
	if err := os.Mkdir(parent, 0o751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o751); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("compiled output"), 0o550); err != nil {
		t.Fatal(err)
	}
	service := &Service{sharedWorkspace: true, sharedUID: os.Geteuid(), sharedGID: os.Getegid()}
	destination := filepath.Join(parent, "new", "nested", "app")
	previous := unix.Umask(0o077)
	err := service.publishBuildOutput(source, destination)
	unix.Umask(previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Dir(destination), filepath.Dir(filepath.Dir(destination))} {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0o770 || info.Mode()&os.ModeSetgid == 0 {
			t.Fatalf("new directory lacks exact shared access: %s %v %v", directory, info, err)
		}
	}
	info, _ := os.Stat(parent)
	if info.Mode().Perm() != 0o751 {
		t.Fatal("existing host directory mode changed")
	}
	info, err = os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o550 {
		t.Fatalf("output mode: %v %v", info, err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(destination, &stat); err != nil || int(stat.Gid) != os.Getegid() {
		t.Fatalf("wrong output group: %+v %v", stat, err)
	}
	if err := os.Chmod(source, 0o440); err != nil {
		t.Fatal(err)
	}
	if err := service.publishBuildOutput(source, destination+".spdx.json"); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(destination + ".spdx.json")
	if info.Mode().Perm() != 0o440 {
		t.Fatal("SBOM gained execute permission")
	}
	link := filepath.Join(parent, "redirect")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := service.publishBuildOutput(source, filepath.Join(link, "escaped")); err == nil {
		t.Fatal("symlink parent was followed")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatal("escaped publication was created")
	}
}
