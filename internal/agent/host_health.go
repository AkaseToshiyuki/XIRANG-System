package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type healthCommand func(context.Context, string, ...string) ([]byte, error)
type hostHealthCollector struct {
	journal         string
	journalDisabled bool
	ipmi            string
	ipmiArgs        []string
	ipmiError       string
	identify        bool
	run             healthCommand
	selListFailure  string
	selListRetryAt  time.Time
}

func configuredHostHealth() *hostHealthCollector {
	collector := &hostHealthCollector{run: runHealthCommand, journalDisabled: os.Getenv("XIRANG_SYSTEM_EVENTS") == "disabled"}
	if !collector.journalDisabled {
		collector.journal, _ = exec.LookPath("journalctl")
	}
	collector.ipmi = strings.TrimSpace(os.Getenv("XIRANG_IPMI_TOOL"))
	host := strings.TrimSpace(os.Getenv("XIRANG_BMC_HOST"))
	if collector.ipmi == "" {
		for _, path := range []string{"/dev/ipmi0", "/dev/ipmi/0", "/dev/ipmidev/0"} {
			if _, err := os.Stat(path); err == nil {
				collector.ipmi, _ = exec.LookPath("ipmitool")
				break
			}
		}
	}
	if collector.ipmi != "" && !filepath.IsAbs(collector.ipmi) {
		collector.ipmiError = "XIRANG_IPMI_TOOL must be an absolute executable path"
	}
	if host != "" {
		user, passwordFile := strings.TrimSpace(os.Getenv("XIRANG_BMC_USERNAME")), strings.TrimSpace(os.Getenv("XIRANG_BMC_PASSWORD_FILE"))
		if net.ParseIP(host) == nil || user == "" || strings.HasPrefix(user, "-") || len(user) > 64 || !filepath.IsAbs(passwordFile) {
			collector.ipmiError = "Remote BMC requires an explicit IP address, username, and protected password file"
		} else if info, err := os.Lstat(passwordFile); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1024 || !healthFileOwned(info) {
			collector.ipmiError = "BMC password file must be a regular file readable only by its owner, up to 1024 bytes"
		} else {
			collector.ipmiArgs = []string{"-I", "lanplus", "-C", "17", "-H", host, "-U", user, "-f", passwordFile, "-N", "2", "-R", "1"}
		}
	} else if os.Getenv("XIRANG_BMC_USERNAME") != "" || os.Getenv("XIRANG_BMC_PASSWORD_FILE") != "" {
		collector.ipmiError = "Remote BMC credentials require XIRANG_BMC_HOST"
	}
	collector.identify = collector.ipmi != "" && collector.ipmiError == "" && os.Getenv("XIRANG_BMC_ALLOW_IDENTIFY") == "true"
	return collector
}

type healthOutput struct {
	bytes.Buffer
	limit int
}

func (output *healthOutput) Write(data []byte) (int, error) {
	if len(data) > output.limit-output.Len() {
		return 0, errors.New("health command output limit exceeded")
	}
	return output.Buffer.Write(data)
}

func runHealthCommand(ctx context.Context, executable string, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, arguments...)
	// Stable language and timestamps; never echo a command, password, or stderr
	// into the public telemetry error fields.
	command.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	command.WaitDelay = time.Second
	stdout, stderr := &healthOutput{limit: 2 << 20}, &healthOutput{limit: 8192}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if err != nil {
		// journalctl --grep returns 1 when there are no matching records.
		var exit *exec.ExitError
		if filepath.Base(executable) == "journalctl" && errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0 && stderr.Len() == 0 {
			return nil, nil
		}
		return stdout.Bytes(), errors.New("command failed, timed out, or exceeded the output limit; check host access and configuration")
	}
	return stdout.Bytes(), nil
}

func (collector *hostHealthCollector) reports(ctx context.Context) <-chan *xirangv1.HostHealthReport {
	output := make(chan *xirangv1.HostHealthReport, 1)
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		defer close(output)
		for {
			report := collector.collect(ctx)
			select {
			case output <- report:
			case <-ctx.Done():
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return output
}

func (collector *hostHealthCollector) collect(ctx context.Context) *xirangv1.HostHealthReport {
	report := &xirangv1.HostHealthReport{ObservedAt: timestamppb.Now(), ChassisPower: "unknown", IdentifySupported: collector.identify}
	journal := &xirangv1.HealthSource{Name: "journal", Status: "unavailable", WindowSeconds: 86400, Limit: 128, Message: "journalctl is unavailable; no system journal was read"}
	if collector.journalDisabled {
		journal.Status, journal.Message = "disabled", "System event collection is disabled by host policy"
	} else if collector.journal != "" {
		journal.Status, journal.Message = "available", "Rolling 24-hour window; at most 64 priority 0–4 records and 64 correction/recovery matches per collection"
		base := []string{"--system", "--no-pager", "--output=json", "--reverse", "--lines=64", "--since=-24h", "--output-fields=MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,_TRANSPORT,_BOOT_ID"}
		seen := map[string]bool{}
		for _, filter := range []string{"--priority=0..4", "--grep=corrected|correctable|uncorrectable|recovered|recovery|repaired|repair completed"} {
			data, err := collector.run(ctx, collector.journal, append(append([]string(nil), base...), filter)...)
			if err != nil {
				journal.Status, journal.Message = "partial", "A journal query failed; records may be missing. Check journal access and journalctl --grep support"
				continue
			}
			events, partial := parseJournalEvents(data)
			if partial || len(events) >= 64 {
				journal.Status = "partial"
			}
			for _, event := range events {
				if !seen[event.SourceRecordId] {
					seen[event.SourceRecordId] = true
					report.Events = append(report.Events, event)
				}
			}
		}
		if journal.Status == "partial" && len(report.Events) == 0 && strings.Contains(journal.Message, "query failed") {
			journal.Status = "unavailable"
		}
	}
	report.Sources = append(report.Sources, journal)
	if collector.ipmi == "" || collector.ipmiError != "" {
		reason := collector.ipmiError
		if reason == "" {
			reason = "No local IPMI device/tool detected and no IPMI tool configured"
		}
		report.Sources = append(report.Sources, &xirangv1.HealthSource{Name: "ipmi", Status: "unavailable", Message: reason})
		return report
	}
	for _, source := range []string{"ipmi-sensors", "ipmi-sel", "ipmi-chassis"} {
		if source == "ipmi-sel" {
			events, status := collector.collectSEL(ctx)
			report.Events = append(report.Events, events...)
			report.Sources = append(report.Sources, status)
			continue
		}
		status := &xirangv1.HealthSource{Name: source, Status: "available"}
		var arguments []string
		switch source {
		case "ipmi-sensors":
			arguments = []string{"-c", "sensor", "list"}
			status.Message = "BMC-reported values and thresholds; sensor accuracy is not calibrated by XIRANG"
		case "ipmi-chassis":
			arguments = []string{"chassis", "power", "status"}
		}
		data, err := collector.run(ctx, collector.ipmi, append(append([]string(nil), collector.ipmiArgs...), arguments...)...)
		if err != nil {
			status.Status, status.Message = "unavailable", err.Error()
		}
		if err == nil || len(data) > 0 {
			if err != nil {
				status.Status = "partial"
			}
			switch source {
			case "ipmi-sensors":
				var partial bool
				report.Sensors, partial = parseIPMISensors(data)
				if partial {
					status.Status = "partial"
				}
			case "ipmi-chassis":
				value := strings.ToLower(strings.TrimSpace(string(data)))
				if value == "chassis power is on" {
					report.ChassisPower = "on"
				} else if value == "chassis power is off" {
					report.ChassisPower = "off"
				} else {
					status.Status, status.Message = "partial", "Unrecognized chassis power response"
				}
			}
		}
		report.Sources = append(report.Sources, status)
	}
	return report
}

func (collector *hostHealthCollector) collectSEL(ctx context.Context) ([]*xirangv1.SystemEvent, *xirangv1.HealthSource) {
	status := &xirangv1.HealthSource{Name: "ipmi-sel", Status: "available", Limit: 128,
		Message: "Latest 128 BMC SEL entries; BMC clock and timezone are unverified, original date/time retained in message"}
	run := func(args ...string) ([]byte, error) {
		return collector.run(ctx, collector.ipmi, append(append([]string(nil), collector.ipmiArgs...), args...)...)
	}
	info, infoErr := run("sel", "info")
	fields := parseIPMIFields(info)
	// Retry an unsuccessful linear walk only when metadata changes or after a
	// bounded cooldown. Continue reading the latest record on every report.
	// The raw info text includes last-add/erase times, count and overflow state;
	// missing or malformed metadata never suppresses a fresh attempt.
	metadataKey := ""
	if _, countErr := strconv.ParseUint(fields["Entries"], 10, 16); infoErr == nil && countErr == nil && fields["Last Add Time"] != "" && fields["Last Del Time"] != "" {
		digest := sha256.Sum256(info)
		metadataKey = hex.EncodeToString(digest[:])
	}
	capacity := ""
	if strings.EqualFold(fields["Overflow"], "true") || strings.HasPrefix(fields["Free Space"], "0 bytes") {
		capacity = "; SEL is full or overflowed: events may be missing and new events may not be recorded; no log was cleared"
	}
	// Even basic `list last` walks older records before producing any output.
	// A full BMC log can exceed the deadline before the requested tail is reached.
	var data []byte
	var err error
	coolingDown := metadataKey != "" && metadataKey == collector.selListFailure && time.Now().Before(collector.selListRetryAt)
	if coolingDown {
		err = errors.New("unchanged SEL list query is in cooldown")
	} else {
		data, err = run("-c", "sel", "list", "last", "128")
		if err != nil && metadataKey != "" {
			collector.selListFailure, collector.selListRetryAt = metadataKey, time.Now().Add(10*time.Minute)
		} else {
			collector.selListFailure, collector.selListRetryAt = "", time.Time{}
		}
	}
	events, partial := parseIPMISEL(data)
	if err != nil {
		status.Message = "SEL list query failed or timed out; returned records may be incomplete. BMC clock and timezone are unverified"
	}
	if err != nil && len(events) == 0 && ctx.Err() == nil {
		latest, latestErr := run("sel", "get", "0xffff")
		if event := parseIPMILatestSEL(latest); latestErr == nil && event != nil {
			events = append(events, event)
			status.Limit = 1
			status.Message = "SEL list query failed or timed out; only the latest record was read directly. Earlier records are missing; BMC clock and timezone are unverified"
		} else {
			status.Message = "SEL list and latest-record queries failed; check host access and BMC responsiveness"
		}
	}
	if err != nil || partial || len(events) >= 128 || infoErr != nil || capacity != "" {
		status.Status = "partial"
	}
	if err != nil && len(events) == 0 {
		status.Status = "unavailable"
	}
	if infoErr != nil {
		status.Message += "; SEL capacity could not be read"
	}
	status.Message += capacity
	return events, status
}

// sel get emits a verbose entry followed by optional SDR details. Only parse
// the first entry; a sensor's current reading is not part of the past event.
func parseIPMIFields(data []byte) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" && len(fields) > 0 {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields
}

func parseIPMILatestSEL(data []byte) *xirangv1.SystemEvent {
	fields := parseIPMIFields(data)
	id, err := strconv.ParseUint(fields["SEL Record ID"], 16, 16)
	if err != nil || id == 0 || id == 0xffff || fields["Record Type"] != "02" || fields["Description"] == "" || fields["Timestamp"] == "" || fields["Sensor Type"] == "" {
		return nil
	}
	direction := fields["Event Direction"]
	if direction != "Assertion Event" && direction != "Deassertion Event" {
		return nil
	}
	message := healthText("SEL #"+fields["SEL Record ID"]+" | "+fields["Timestamp"]+" | "+fields["Sensor Type"]+" #"+fields["Sensor Number"]+" | "+fields["Description"]+" | "+direction, 2048)
	kind, severity := classifySystemEvent(message, 6)
	if direction == "Deassertion Event" {
		kind, severity = "recovery", "info"
	}
	digest := sha256.Sum256([]byte("latest-sel\x00" + message + "\x00" + fields["Event Data"]))
	return &xirangv1.SystemEvent{Source: "ipmi-sel", SourceRecordId: hex.EncodeToString(digest[:]), Message: message, Component: healthText(fields["Sensor Type"], 256), Kind: kind, Severity: severity}
}

func (collector *hostHealthCollector) identifyCommand(ctx context.Context, command *xirangv1.SetBMCIdentify) (bool, string) {
	if !collector.identify {
		return false, "BMC locator control is disabled by host policy"
	}
	if command == nil || command.ExpiresAt == nil || command.ExpiresAt.CheckValid() != nil || !command.ExpiresAt.AsTime().After(time.Now()) || command.ExpiresAt.AsTime().After(time.Now().Add(2*time.Minute)) {
		return false, "BMC locator command expired or has an invalid deadline"
	}
	seconds := "0"
	if command.Enabled {
		seconds = "120"
	}
	probe, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	_, err := collector.run(probe, collector.ipmi, append(append([]string(nil), collector.ipmiArgs...), "chassis", "identify", seconds)...)
	if err != nil {
		return false, "BMC locator command failed; check host configuration"
	}
	return true, "BMC acknowledged the locator request; physical LED state is not independently verified"
}

var correctedEvent = regexp.MustCompile(`(?i)\b(corrected|correctable)\b`)
var failedEvent = regexp.MustCompile(`(?i)\b(uncorrected|uncorrectable|fatal|panic|failed|failure)\b`)
var recoveryEvent = regexp.MustCompile(`(?i)\b(recovered|recovery complete[d]?|repaired|repair complete[d]?)\b`)

func classifySystemEvent(message string, priority int) (string, string) {
	if failedEvent.MatchString(message) {
		return "error", "critical"
	}
	if correctedEvent.MatchString(message) {
		return "corrected", "warning"
	}
	if recoveryEvent.MatchString(message) {
		return "recovery", "info"
	}
	if priority <= 2 {
		return "error", "critical"
	}
	if priority <= 4 {
		return "error", "warning"
	}
	return "info", "info"
}

func parseJournalEvents(data []byte) ([]*xirangv1.SystemEvent, bool) {
	events := []*xirangv1.SystemEvent{}
	partial := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 128<<10)
	for scanner.Scan() {
		var fields map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &fields) != nil {
			partial = true
			continue
		}
		read := func(key string) string { var value string; _ = json.Unmarshal(fields[key], &value); return value }
		message, cursor := healthText(read("MESSAGE"), 2048), read("__CURSOR")
		if message == "" || cursor == "" || len(cursor) > 1024 {
			partial = true
			continue
		}
		priority, err := strconv.Atoi(read("PRIORITY"))
		if err != nil {
			priority = 6
		}
		kind, severity := classifySystemEvent(message, priority)
		component := read("_SYSTEMD_UNIT")
		if component == "" {
			component = read("SYSLOG_IDENTIFIER")
		}
		if component == "" {
			component = read("_TRANSPORT")
		}
		event := &xirangv1.SystemEvent{Source: "journal", SourceRecordId: cursor, Message: message, Kind: kind, Severity: severity, Component: healthText(component, 256)}
		if micros, err := strconv.ParseInt(read("__REALTIME_TIMESTAMP"), 10, 64); err == nil && micros > 0 {
			event.OccurredAt = timestamppb.New(time.UnixMicro(micros))
		}
		events = append(events, event)
		if len(events) >= 64 {
			break
		}
	}
	return events, partial || scanner.Err() != nil
}

func parseIPMISensors(data []byte) ([]*xirangv1.HardwareSensor, bool) {
	items := []*xirangv1.HardwareSensor{}
	partial := false
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	for {
		fields, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			partial = true
			break
		}
		if len(fields) != 10 {
			partial = true
			continue
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if fields[0] == "" {
			partial = true
			continue
		}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", len(items), fields[0])))
		value, available := finiteSensorValue(fields[1])
		if fields[2] == "discrete" {
			available = false
		}
		if !available {
			value = 0
		}
		sensor := &xirangv1.HardwareSensor{Id: hex.EncodeToString(digest[:16]), Name: healthText(fields[0], 128), Source: "ipmi", Value: value, Available: available, Unit: healthText(fields[2], 64), Status: healthText(fields[3], 128)}
		if value, ok := finiteSensorValue(fields[5]); ok {
			sensor.LowerCritical = &value
		}
		if value, ok := finiteSensorValue(fields[8]); ok {
			sensor.UpperCritical = &value
		}
		items = append(items, sensor)
		if len(items) >= hosthealth.MaxSensors {
			partial = true
			break
		}
	}
	return items, partial
}

func finiteSensorValue(value string) (float64, bool) {
	number, err := strconv.ParseFloat(value, 64)
	return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func parseIPMISEL(data []byte) ([]*xirangv1.SystemEvent, bool) {
	events := []*xirangv1.SystemEvent{}
	partial := false
	if strings.TrimSpace(string(data)) == "SEL has no entries" {
		return events, false
	}
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	for {
		fields, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			partial = true
			break
		}
		if len(fields) < 4 {
			partial = true
			continue
		}
		if _, err := strconv.ParseUint(strings.TrimSpace(fields[0]), 16, 16); err != nil {
			partial = true
			continue
		}
		message := healthText("SEL #"+fields[0]+" | "+strings.Join(fields[1:], " | "), 2048)
		kind, severity := classifySystemEvent(message, 6)
		// SEL deassertion means the BMC stopped asserting this condition. It
		// does not establish that a technician repaired its cause.
		if strings.EqualFold(strings.TrimSpace(fields[len(fields)-1]), "Deasserted") {
			kind, severity = "recovery", "info"
		}
		if kind == "info" && strings.Contains(strings.ToLower(message), "critical") {
			kind, severity = "error", "critical"
			if strings.Contains(strings.ToLower(message), "non-critical") {
				severity = "warning"
			}
		}
		digest := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
		events = append(events, &xirangv1.SystemEvent{Source: "ipmi-sel", SourceRecordId: hex.EncodeToString(digest[:]), Message: message, Component: healthText(fields[3], 256), Kind: kind, Severity: severity})
		if len(events) >= 128 {
			break
		}
	}
	return events, partial
}

func healthText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(value, "�"))
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func healthFileOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
