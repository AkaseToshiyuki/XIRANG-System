package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
)

var journalFilters = []string{"--priority=0..4", "--grep=corrected|correctable|uncorrectable|recovered|recovery|repaired|repair completed"}

type journalPage struct {
	events  []*xirangv1.SystemEvent
	first   string
	last    string
	rows    int
	skipped uint64
}

// Cursors are opaque systemd strings. Unsupported MESSAGE encodings are
// accounted for as skipped records; malformed JSON/cursors stop advancement.
func readJournalPage(data []byte) (journalPage, error) {
	page := journalPage{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 128<<10)
	for scanner.Scan() {
		var row struct {
			Cursor string `json:"__CURSOR"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil || row.Cursor == "" || len(row.Cursor) > 1024 || !utf8.ValidString(row.Cursor) || strings.ContainsRune(row.Cursor, 0) {
			return page, errors.New("journal page contains an invalid cursor or JSON record")
		}
		if page.rows == 0 {
			page.first = row.Cursor
		}
		page.last = row.Cursor
		page.rows++
		if page.rows > 65 {
			return page, errors.New("journal page exceeds the requested bound")
		}
		events, partial := parseJournalEvents(scanner.Bytes())
		if partial || len(events) != 1 {
			page.skipped++
			continue
		}
		event := events[0]
		if event.OccurredAt != nil && (event.OccurredAt.CheckValid() != nil || event.OccurredAt.AsTime().After(time.Now().Add(24*time.Hour))) {
			event.OccurredAt = nil
			page.skipped++
		}
		page.events = append(page.events, event)
	}
	return page, scanner.Err()
}

func (collector *hostHealthCollector) collectJournalIncremental(ctx context.Context) ([]*xirangv1.SystemEvent, *xirangv1.HealthSource) {
	source := &xirangv1.HealthSource{Name: "journal", Status: "available", Limit: 128,
		Message: "Persistent cursors; first scan starts within 24 hours, then up to 64 oldest new matches per filter per minute; source retention still limits coverage"}
	base := []string{"--system", "--no-pager", "--output=json", "--output-fields=MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,_TRANSPORT,_BOOT_ID"}
	var events []*xirangv1.SystemEvent
	seen := map[string]bool{}
	for index, filter := range journalFilters {
		cursor := collector.journalState.Cursors[index]
		arguments := append([]string(nil), base...)
		if cursor == "" {
			arguments = append(arguments, "--since=-24h", "--lines=+64")
		} else {
			arguments = append(arguments, "--cursor="+cursor, "--lines=+65")
		}
		arguments = append(arguments, filter)
		data, err := collector.run(ctx, collector.journal, arguments...)
		if err != nil {
			source.Status, source.Message = "partial", "Journal query failed; cursor retained for retry; access or source retention may limit coverage"
			continue
		}
		page, err := readJournalPage(data)
		if err != nil {
			source.Status, source.Message = "partial", "Malformed or oversized journal page; cursor retained for retry"
			continue
		}
		if cursor != "" && page.first != cursor {
			// journalctl seeks to a nearby entry when a cursor was vacuumed.
			// Do not silently accept that seek as continuous coverage.
			collector.journalState.Gaps++
			collector.journalState.Cursors[index] = ""
			source.Status, source.Message = "partial", "Journal cursor disappeared or no longer matches; coverage gap recorded; next scan restarts within 24 hours"
			continue
		}
		if page.last != "" {
			collector.journalState.Cursors[index] = page.last
		}
		collector.journalState.Gaps += page.skipped
		if page.skipped > 0 || (cursor == "" && page.rows >= 64) || page.rows >= 65 {
			source.Status = "partial"
		}
		for _, event := range page.events {
			if event.SourceRecordId != cursor && !seen[event.SourceRecordId] {
				seen[event.SourceRecordId] = true
				events = append(events, event)
			}
		}
	}
	if collector.journalState.Gaps > 0 {
		source.Status = "partial"
	}
	return events, source
}
