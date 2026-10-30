package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Import a documented export stream with the upstream writer, then use real
// journalctl to exercise page boundaries and file rotation. All writes are to
// temporary files; the test never signals or vacuums the host's journal service.
func TestJournalPaginationRotationAndVacuumWithSystemd(t *testing.T) {
	remote := os.Getenv("XIRANG_JOURNAL_REMOTE")
	if remote == "" {
		t.Skip("set XIRANG_JOURNAL_REMOTE to an independent systemd-journal-remote executable")
	}
	if !filepath.IsAbs(remote) {
		t.Fatal("journal writer path must be absolute")
	}
	journalctl, err := exec.LookPath("journalctl")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	start := time.Now().Add(-time.Hour).UnixMicro()
	write := func(name string, first, end int) string {
		t.Helper()
		var data strings.Builder
		for i := first; i < end; i++ {
			fmt.Fprintf(&data, "__REALTIME_TIMESTAMP=%d\n__MONOTONIC_TIMESTAMP=%d\n_BOOT_ID=12345678901234567890123456789012\n_MACHINE_ID=abcdefabcdefabcdefabcdefabcdefab\n_SYSTEMD_UNIT=fixture.service\nPRIORITY=4\nMESSAGE=corrected ECC fixture-%03d\n\n", start+int64(i)*1_000_000, 1_000_000+int64(i)*1_000_000, i)
		}
		path := filepath.Join(directory, name)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, remote, "--output="+path, "--split-mode=none", "-")
		command.Stdin = strings.NewReader(data.String())
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("upstream journal import: %v %s", err, output)
		}
		return path
	}
	firstFile := write("first.journal", 0, 140)
	state := &journalCheckpoint{}
	collector := &hostHealthCollector{journal: journalctl, journalState: state, run: func(ctx context.Context, executable string, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "--system" {
				args[i] = "--directory=" + directory
			}
		}
		return runHealthCommand(ctx, executable, args...)
	}}
	var messages []string
	for _, want := range []int{64, 64, 12} {
		events, source := collector.collectJournalIncremental(context.Background())
		if len(events) != want {
			t.Fatalf("forward page got=%d want=%d source=%+v", len(events), want, source)
		}
		for _, event := range events {
			messages = append(messages, event.Message)
		}
	}
	for i, message := range messages {
		if message != fmt.Sprintf("corrected ECC fixture-%03d", i) {
			t.Fatalf("page skipped or duplicated index %d", i)
		}
	}
	if state.Gaps != 0 {
		t.Fatal("normal pagination invented gaps")
	}
	beforeRotation := *state
	rotated := filepath.Join(directory, "rotated.journal")
	if err := os.Rename(firstFile, rotated); err != nil {
		t.Fatal(err)
	}
	write("second.journal", 140, 150)
	events, source := collector.collectJournalIncremental(context.Background())
	if len(events) != 10 || state.Gaps != 0 || events[0].Message != "corrected ECC fixture-140" {
		t.Fatalf("rotation lost continuity: events=%d gaps=%d source=%+v", len(events), state.Gaps, source)
	}
	// Model an offline reader whose saved cursor was in a vacuumed journal.
	*state = beforeRotation
	if err := os.Remove(rotated); err != nil {
		t.Fatal(err)
	}
	events, source = collector.collectJournalIncremental(context.Background())
	if len(events) != 0 || state.Gaps != 2 || state.Cursors != [2]string{} || source.Status != "partial" {
		t.Fatalf("vacuum gap not reported: events=%d state=%+v source=%+v", len(events), state, source)
	}
	events, source = collector.collectJournalIncremental(context.Background())
	if len(events) != 10 || state.Gaps != 2 || source.Status != "partial" {
		t.Fatal("bounded restart hid a previous gap or lost remaining records")
	}
}
