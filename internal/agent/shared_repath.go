package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/networkhealth"
)

func nfsSource(host, path string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + path
}

// A failed/timed-out switch must not let new work write into the local mount
// placeholder. Read the Agent's own namespace each time, including after a new
// control session or process restart; a previous acknowledgement is insufficient.
func verifySharedMountsForWork() error {
	fstab, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	return checkWorkMounts(fstab, readHostMount)
}

func checkWorkMounts(fstab []byte, read func(string) (hostMount, bool, error)) error {
	for _, line := range strings.Split(string(fstab), "\n") {
		entry, marker, found := strings.Cut(line, "#")
		if !found || !strings.HasPrefix(strings.TrimSpace(marker), "xirang:") {
			continue
		}
		fields := strings.Fields(entry)
		if len(fields) != 6 || !safeMountPath(fields[1]) || fields[2] != "nfs" && fields[2] != "nfs4" {
			return errors.New("managed shared mount configuration is invalid")
		}
		mount, present, err := read(fields[1])
		if err != nil {
			return err
		}
		if !present || mount.source != fields[0] || mount.kind != "nfs" && mount.kind != "nfs4" {
			return errors.New("shared network mount is unavailable; wait for reconciliation")
		}
	}
	return nil
}

// A normal unmount is the busy check. Never use lazy/forced unmounts or replace
// another owner's filesystem. Failed mounts restore the preceding source.
func switchNFSMount(target, previous, desired string, read func(string) (hostMount, bool, error), run func(string, ...string) error) error {
	current, present, err := read(target)
	if err != nil {
		return err
	}
	if present && (current.kind != "nfs" && current.kind != "nfs4" || current.source != previous && current.source != desired) {
		return errors.New("shared path contains an unrelated mount")
	}
	if present && current.source == desired {
		return nil
	}
	if present {
		if err := run("umount", target); err != nil {
			return fmt.Errorf("shared path change deferred: %w", err)
		}
	}
	err = run("mount", "-t", "nfs", "-o", "rw,hard,timeo=60,retrans=2", desired, target)
	if err == nil {
		current, present, err = read(target)
		if err == nil && (!present || current.source != desired || current.kind != "nfs" && current.kind != "nfs4") {
			err = errors.New("selected NFS source was not observed")
		}
	}
	if err != nil && previous != "" {
		_, present, readErr := read(target)
		if readErr == nil && !present {
			err = errors.Join(err, run("mount", "-t", "nfs", "-o", "rw,hard,timeo=60,retrans=2", previous, target))
		}
	}
	return err
}

func repathSharedMount(arguments []string) error {
	if len(arguments) != 7 || net.ParseIP(arguments[2]) == nil || net.ParseIP(arguments[5]) == nil ||
		!safeMountPath(arguments[3]) || !safeMountPath(arguments[4]) || !networkhealth.ValidInterface(arguments[6]) {
		return errors.New("invalid shared path selection arguments")
	}
	id, host, sourcePath, target, client, iface := arguments[1], arguments[2], arguments[3], arguments[4], arguments[5], arguments[6]
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ipTool, err := fixedMountTool("ip")
	if err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, ipTool, "-j", "route", "get", host).Output()
	if err != nil {
		return fmt.Errorf("selected shared path unavailable: %w", err)
	}
	if err = networkhealth.CheckRouteJSON(output, iface); err != nil {
		return fmt.Errorf("selected shared path unavailable: %w", err)
	}
	var routes []struct {
		Source string `json:"prefsrc"`
	}
	if json.Unmarshal(output, &routes) != nil || len(routes) != 1 || routes[0].Source != client {
		return errors.New("selected shared path unavailable: NFS source route differs from selected client address")
	}
	// Check that the selected service accepts TCP before withdrawing a mount.
	dialer := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(client)}}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "2049"))
	if err != nil {
		return fmt.Errorf("selected shared path unavailable: %w", err)
	}
	connection.Close()
	desired := nfsSource(host, sourcePath)
	current, present, err := readHostMount(target)
	if err != nil {
		return err
	}
	var previous string
	if present {
		fstab, err := os.ReadFile("/etc/fstab")
		if err != nil {
			return err
		}
		previous, err = ownedFSTabSource(fstab, id, target)
		if err != nil {
			return err
		}
		if previous != current.source || !strings.HasSuffix(previous, ":"+sourcePath) {
			return errors.New("shared path ownership or source directory changed")
		}
	}
	if !present {
		if err := updateSystemdSharedPathPolicy(id+"-mount", target, true); err != nil {
			return err
		}
		directory, err := openOrCreateSharedDirectory(target)
		if err != nil {
			return err
		}
		directory.Close()
	}
	if err := switchNFSMount(target, previous, desired, readHostMount, runMountCommand); err != nil {
		return err
	}
	if err := putFSTab(id, desired, target); err != nil {
		if previous != "" {
			err = errors.Join(err, switchNFSMount(target, desired, previous, readHostMount, runMountCommand))
		}
		return err
	}
	return nil
}
