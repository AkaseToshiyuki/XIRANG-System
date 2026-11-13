package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// SSH file helpers may operate only on an export registered by the privileged
// Agent. A permitted file action is not authority to choose an arbitrary host
// root such as /etc or the Agent's credential directory.
func requireExportedSharedRoot(id, path string) error {
	contents, err := readManagedSharedExport(id, path)
	if err != nil {
		return err
	}
	if sharedExportRequiresMount(contents) {
		return requireSharedSourceMount(path)
	}
	return nil
}

func readManagedSharedExport(id, path string) (string, error) {
	if !safeMountID(id) || !safeMountPath(path) {
		return "", errors.New("invalid shared export identity or path")
	}
	fd, err := unix.Open("/etc/exports.d", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != 0 || stat.Mode&0o022 != 0 {
		return "", errors.New("shared export directory must be root-owned and protected")
	}
	exportFD, err := unix.Openat(fd, "xirang-"+id+".exports", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("shared source is not registered as an export: %w", err)
	}
	file := os.NewFile(uintptr(exportFD), "shared export")
	defer file.Close()
	if err := unix.Fstat(exportFD, &stat); err != nil || stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o022 != 0 {
		return "", errors.New("shared export configuration must be a protected root-owned file")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(contents) > 8192 {
		return "", errors.New("invalid shared export configuration")
	}
	if err := validateSharedExport(string(contents), path); err != nil {
		return "", err
	}
	return string(contents), nil
}

func sharedExportRequiresMount(contents string) bool {
	// Only the managed, exact-source form is supported. validateSharedExport
	// rejects alternate path forms so rewriting cannot silently discard them.
	fields := strings.Fields(contents)
	if len(fields) != 2 {
		return false
	}
	options := "," + strings.TrimSuffix(strings.TrimPrefix(fields[1], "*("), ")") + ","
	return strings.Contains(options, ",mountpoint,") || strings.Contains(options, ",mp,")
}

func requireSharedSourceMount(path string) error {
	_, mounted, err := readHostMount(path)
	if err != nil {
		return err
	}
	if !mounted {
		return errors.New("shared source filesystem is not mounted")
	}
	return nil
}

func validateSharedExport(contents, path string) error {
	lines := strings.Split(strings.TrimSpace(contents), "\n")
	if len(lines) != 1 {
		return errors.New("shared export configuration must contain one managed source")
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[0] != path || !strings.HasPrefix(fields[1], "*(") || !strings.HasSuffix(fields[1], ")") {
		return errors.New("requested source does not match the registered shared export")
	}
	options := "," + strings.TrimSuffix(strings.TrimPrefix(fields[1], "*("), ")") + ","
	if strings.Contains(options, ",mountpoint=") || strings.Contains(options, ",mp=") {
		return errors.New("managed shared exports require the exact-source mountpoint option")
	}
	if !strings.Contains(options, ",all_squash,") || strings.Contains(options, ",no_all_squash,") || strings.Contains(options, ",no_root_squash,") {
		return errors.New("managed shared exports require all_squash")
	}
	return nil
}

func dropFileQuotaBypass() error {
	header := unix.CapUserHeader{Version: 0x20080522} // _LINUX_CAPABILITY_VERSION_3
	var data [2]unix.CapUserData
	if err := unix.Capget(&header, &data[0]); err != nil {
		return err
	}
	index, bit := unix.CAP_SYS_RESOURCE/32, uint32(1)<<uint(unix.CAP_SYS_RESOURCE%32)
	data[index].Effective &^= bit
	data[index].Permitted &^= bit
	data[index].Inheritable &^= bit
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("cannot enforce filesystem quota for shared file operation: %w", err)
	}
	return nil
}
