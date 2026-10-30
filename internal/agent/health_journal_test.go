package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func journalTestRow(cursor, message string) string {
	data, _ := json.Marshal(map[string]string{"__CURSOR": cursor, "MESSAGE": message, "PRIORITY": "4", "__REALTIME_TIMESTAMP": "1760000000000000"})
	return string(data) + "\n"
}

func TestJournalCursorForwardPaginationAndCoverageGaps(t *testing.T) {
	state := &journalCheckpoint{Cursors: [2]string{"anchor", ""}}
	mode := "normal"
	collector := &hostHealthCollector{journal: "journalctl", journalState: state, run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "--reverse") || !strings.Contains(joined, "--lines=+") {
			t.Fatal("query would skip oldest unseen records")
		}
		if strings.Contains(joined, "--grep=") {
			return nil, nil
		}
		switch mode {
		case "normal":
			if !strings.Contains(joined, "--cursor=anchor") || strings.Contains(joined, "--since=") {
				t.Fatal("saved cursor was replaced by rolling window")
			}
			return []byte(journalTestRow("anchor", "old") + journalTestRow("next", "corrected ECC")), nil
		case "rotated":
			return []byte(journalTestRow("unrelated", "vacuumed anchor")), nil
		case "failed":
			return []byte(journalTestRow("partial", "partial command output")), errors.New("timeout")
		case "malformed":
			return []byte(journalTestRow("anchor", "old") + "{truncated"), nil
		case "unsupported":
			return []byte("{\"__CURSOR\":\"binary\",\"MESSAGE\":[0,255],\"PRIORITY\":\"4\"}\n"), nil
		}
		return nil, nil
	}}
	events, status := collector.collectJournalIncremental(context.Background())
	if len(events) != 1 || events[0].SourceRecordId != "next" || state.Cursors[0] != "next" || status.Status != "available" {
		t.Fatalf("pagination: %v %+v %+v", events, state, status)
	}
	mode = "failed"
	events, status = collector.collectJournalIncremental(context.Background())
	if len(events) != 0 || state.Cursors[0] != "next" || status.Status != "partial" {
		t.Fatal("failed command advanced cursor")
	}
	mode = "malformed"
	_, status = collector.collectJournalIncremental(context.Background())
	if state.Cursors[0] != "next" || status.Status != "partial" {
		t.Fatal("malformed page advanced cursor")
	}
	mode = "rotated"
	events, status = collector.collectJournalIncremental(context.Background())
	if len(events) != 0 || state.Cursors[0] != "" || state.Gaps != 1 || status.Status != "partial" {
		t.Fatal("missing cursor silently skipped records")
	}
	mode = "unsupported"
	_, status = collector.collectJournalIncremental(context.Background())
	if state.Cursors[0] != "binary" || state.Gaps != 2 || status.Status != "partial" {
		t.Fatal("unsupported record was not accounted for")
	}
}

// Explicitly enabled: read-only use of the installed independent journalctl.
// It prints no messages, identifiers or cursors, and does not modify journals.
func TestJournalCursorWithInstalledSystemd(t *testing.T) {
	if os.Getenv("XIRANG_TEST_SYSTEM_JOURNAL") != "1" {
		t.Skip("set XIRANG_TEST_SYSTEM_JOURNAL=1 for read-only journalctl verification")
	}
	path, err := exec.LookPath("journalctl")
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"--system", "--no-pager", "--output=json", "--output-fields=MESSAGE,PRIORITY", "--priority=0..4"}
	data, err := runHealthCommand(context.Background(), path, append(append([]string(nil), base...), "--lines=+2")...)
	if err != nil {
		t.Fatal(err)
	}
	page, err := readJournalPage(data)
	if err != nil || page.rows < 2 {
		t.Fatalf("requires at least two readable journal records; rows=%d err=%v", page.rows, err)
	}
	data, err = runHealthCommand(context.Background(), path, append(append([]string(nil), base...), "--cursor="+page.first, "--lines=+2")...)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := readJournalPage(data)
	if err != nil || resumed.rows != 2 || resumed.first != page.first || resumed.last != page.last {
		t.Fatal("installed journalctl did not resume inclusively in forward order")
	}
}
