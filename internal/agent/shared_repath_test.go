package agent

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNFSSwitchPreservesBusyAndForeignMounts(t *testing.T) {
	for _, kind := range []string{"busy", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			current := hostMount{source: "old:/data", kind: "nfs4"}
			if kind == "foreign" {
				current.source = "unrelated:/data"
			}
			var commands []string
			err := switchNFSMount("/mount", "old:/data", "new:/data", func(string) (hostMount, bool, error) { return current, true, nil }, func(cmd string, args ...string) error { commands = append(commands, cmd); return errors.New("busy") })
			if err == nil {
				t.Fatal("unsafe switch succeeded")
			}
			if kind == "foreign" && len(commands) != 0 || kind == "busy" && !reflect.DeepEqual(commands, []string{"umount"}) {
				t.Fatalf("unexpected commands: %v", commands)
			}
		})
	}
}

func TestNFSSwitchFailureRestoresOriginalAndNeverForcesUnmount(t *testing.T) {
	current := hostMount{source: "old:/data", kind: "nfs4"}
	present := true
	var sources []string
	err := switchNFSMount("/mount", "old:/data", "new:/data", func(string) (hostMount, bool, error) { return current, present, nil }, func(cmd string, args ...string) error {
		if cmd == "umount" {
			if !reflect.DeepEqual(args, []string{"/mount"}) {
				t.Fatalf("unsafe unmount %v", args)
			}
			present = false
			return nil
		}
		source := args[len(args)-2]
		sources = append(sources, source)
		if strings.HasPrefix(source, "new:") {
			return errors.New("new path refused")
		}
		current.source = source
		present = true
		return nil
	})
	if err == nil || !present || current.source != "old:/data" || !reflect.DeepEqual(sources, []string{"new:/data", "old:/data"}) {
		t.Fatalf("old access not restored: %+v %v %v", current, sources, err)
	}
}

func TestNFSSwitchVerifiesNewSourceAndIsIdempotent(t *testing.T) {
	current := hostMount{source: "old:/data", kind: "nfs4"}
	present := true
	calls := 0
	read := func(string) (hostMount, bool, error) { return current, present, nil }
	run := func(cmd string, args ...string) error {
		calls++
		if cmd == "umount" {
			present = false
		} else {
			current.source = args[len(args)-2]
			present = true
		}
		return nil
	}
	if err := switchNFSMount("/mount", "old:/data", "new:/data", read, run); err != nil {
		t.Fatal(err)
	}
	if err := switchNFSMount("/mount", "old:/data", "new:/data", read, run); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || current.source != "new:/data" {
		t.Fatalf("unexpected mount mutation: %d %+v", calls, current)
	}
	if nfsSource("2001:db8::1", "/data") != "[2001:db8::1]:/data" {
		t.Fatal("invalid IPv6 NFS source")
	}
}

func TestWorkCannotUseUnmountedPlaceholderOrUnconfirmedNamespace(t *testing.T) {
	fstab := []byte("UUID=root / ext4 defaults 0 1\n192.0.2.1:/share /srv/share nfs rw 0 0 # xirang:share\n")
	for _, test := range []struct {
		name    string
		mount   hostMount
		present bool
		valid   bool
	}{
		{"confirmed", hostMount{source: "192.0.2.1:/share", kind: "nfs4"}, true, true},
		{"placeholder", hostMount{}, false, false},
		{"old_namespace", hostMount{source: "198.51.100.1:/share", kind: "nfs4"}, true, false},
		{"local_mount", hostMount{source: "192.0.2.1:/share", kind: "ext4"}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkWorkMounts(fstab, func(target string) (hostMount, bool, error) {
				if target != "/srv/share" {
					t.Fatalf("inspected unmanaged path %s", target)
				}
				return test.mount, test.present, nil
			})
			if (err == nil) != test.valid {
				t.Fatalf("mount readiness: %v", err)
			}
		})
	}
}
