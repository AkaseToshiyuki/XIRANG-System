package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharedMountFSTabOwnershipIsExact(t *testing.T) {
	input := "# system header\n/dev/root / ext4 defaults 0 1\nserver:/data /mnt/data nfs rw 0 0 # xirang:share\nserver:/other /mnt/other nfs rw 0 0 # xirang:share_other\n# keep xirang:share for operators\n"
	expected := "# system header\n/dev/root / ext4 defaults 0 1\nserver:/other /mnt/other nfs rw 0 0 # xirang:share_other\n# keep xirang:share for operators\n"
	if got := string(withoutFSTabID([]byte(input), "share")); got != expected {
		t.Fatalf("changed unrelated fstab entry: %q", got)
	}
	if source, err := ownedFSTabSource([]byte(input), "share", "/mnt/data"); err != nil || source != "server:/data" {
		t.Fatal(source, err)
	}
	for _, test := range []struct{ input, id, target string }{
		{input, "share", "/mnt/other"}, {input, "missing", "/mnt/data"},
		{input + "server:/data /mnt/data nfs rw 0 0 # xirang:share\n", "share", "/mnt/data"},
		{strings.Replace(input, "/mnt/data nfs", "/mnt/data ext4", 1), "share", "/mnt/data"},
	} {
		if _, err := ownedFSTabSource([]byte(test.input), test.id, test.target); err == nil {
			t.Fatal("ambiguous/unowned unmount accepted")
		}
	}
}

func TestHostMountParserKernelFormatAndAmbiguity(t *testing.T) {
	// Format from Linux proc documentation, including unknown optional tags.
	input := "36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue\n37 35 0:99 / /mnt/data rw shared:12 future:1 - nfs4 server:/data rw,vers=4.2\n"
	got, found, err := parseHostMount(strings.NewReader(input), "/mnt/data")
	if err != nil || !found || got.source != "server:/data" || got.kind != "nfs4" {
		t.Fatal(got, found, err)
	}
	if _, found, err := parseHostMount(strings.NewReader(input), "/mnt/missing"); err != nil || found {
		t.Fatal("parent mistaken for exact mountpoint")
	}
	if _, _, err := parseHostMount(strings.NewReader(input+input), "/mnt/data"); err == nil {
		t.Fatal("stacked mounts treated as one")
	}
	if _, _, err := parseHostMount(strings.NewReader("malformed"), "/mnt/data"); err == nil {
		t.Fatal("malformed input accepted")
	}
}

func TestHostMountParserAgainstInstalledFindmnt(t *testing.T) {
	binary, err := exec.LookPath("findmnt")
	if err != nil {
		t.Skip("independent util-linux findmnt unavailable")
	}
	for _, target := range []string{"/", "/proc"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		output, err := exec.CommandContext(ctx, binary, "--kernel", "--json", "--nofsroot", "--mountpoint", target, "--output", "TARGET,SOURCE,FSTYPE").Output()
		cancel()
		if err != nil {
			t.Fatal("independent findmnt failed", err)
		}
		var reference struct {
			Filesystems []struct {
				Source string `json:"source"`
				Kind   string `json:"fstype"`
				Target string `json:"target"`
			} `json:"filesystems"`
		}
		if err := json.Unmarshal(output, &reference); err != nil || len(reference.Filesystems) != 1 {
			t.Fatal("invalid independent mount reference")
		}
		got, found, err := readHostMount(target)
		if err != nil || !found || reference.Filesystems[0].Source != got.source || reference.Filesystems[0].Kind != got.kind {
			t.Fatal("mount parser disagrees with util-linux")
		}
	}
}

func TestMountConfigurationReplacementPreservesOtherFilesAndRejectsLinks(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fstab")
	if err := writeMountConfiguration(path, []byte("original\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeMountConfiguration(path, []byte("replacement\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement\n" {
		t.Fatal("replacement failed")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary file leaked")
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeMountConfiguration(link, []byte("bad")); err == nil {
		t.Fatal("linked configuration accepted")
	}
	data, _ = os.ReadFile(path)
	if string(data) != "replacement\n" {
		t.Fatal("link target modified")
	}
}
