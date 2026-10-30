package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Column order follows upstream ipmitool's dump_sensor_fc_thredshold_csv
// (lib/ipmi_sensor.c), not the parser: id,value,units,status,lnr,lcr,lnc,unc,ucr,unr.
func TestIPMISensorsPreserveValuesThresholdsAndUnavailableState(t *testing.T) {
	data := "CPU1 Temp,47.125,degrees C,ok,na,5.000,10.000,80.000,90.000,100.000\n" +
		"FAN1,4200.000,RPM,ok,na,800.000,1200.000,na,na,na\n" +
		"PSU Power,0.000,Watts,ok,na,na,na,na,1000.000,na\n" +
		"Absent,na,degrees C,na,na,na,na,na,na,na\n" +
		"Bad,NaN,Volts,na,na,na,na,na,+Inf,na\n" +
		"Subzero,-4.500,degrees C,ok,na,-10,na,na,90,na\n"
	sensors, partial := parseIPMISensors([]byte(data))
	if partial || len(sensors) != 6 {
		t.Fatalf("sensors=%#v partial=%t", sensors, partial)
	}
	if sensors[0].Value != 47.125 || *sensors[0].LowerCritical != 5 || *sensors[0].UpperCritical != 90 {
		t.Fatalf("wrong columns: %+v", sensors[0])
	}
	if !sensors[2].Available || sensors[2].Value != 0 || sensors[3].Available || sensors[4].Available || sensors[4].UpperCritical != nil || sensors[5].Value != -4.5 {
		t.Fatal("zero, missing, nonfinite, or negative reading was confused")
	}
	if _, partial := parseIPMISensors([]byte("malformed\n")); !partial {
		t.Fatal("malformed format was reported complete")
	}
}

func TestJournalClassificationAndMalformedMessages(t *testing.T) {
	lines := []string{
		`{"__CURSOR":"s=1;i=1","__REALTIME_TIMESTAMP":"1793322000000000","PRIORITY":"4","MESSAGE":"EDAC MC0: 1 corrected error","_TRANSPORT":"kernel"}`,
		`{"__CURSOR":"s=1;i=2","__REALTIME_TIMESTAMP":"1793322001000000","PRIORITY":"3","MESSAGE":"Uncorrectable ECC error"}`,
		`{"__CURSOR":"s=1;i=3","PRIORITY":"6","MESSAGE":"Device recovered"}`,
		`{"__CURSOR":"s=1;i=4","PRIORITY":"3","MESSAGE":"recovery failed"}`,
		`{"__CURSOR":"s=1;i=5","MESSAGE":[255,0,10]}`,
	}
	events, partial := parseJournalEvents([]byte(strings.Join(lines, "\n")))
	if !partial || len(events) != 4 {
		t.Fatalf("events=%v partial=%t", events, partial)
	}
	for i, kind := range []string{"corrected", "error", "recovery", "error"} {
		if events[i].Kind != kind {
			t.Errorf("%q: got %s want %s", events[i].Message, events[i].Kind, kind)
		}
	}
	if events[0].OccurredAt.AsTime().UnixMicro() != 1793322000000000 || events[2].OccurredAt != nil {
		t.Fatal("timestamp precision or missing timestamp was fabricated")
	}
}

// SEL CSV record/date/time/sensor/description/direction comes from upstream
// ipmi_sel_print_std_entry (lib/ipmi_sel.c). No trustworthy BMC timezone is given.
func TestSELDeassertionCorrectionAndNormalAssertion(t *testing.T) {
	data := "1,10/30/2026,10:00:00,Memory DIMM1,Correctable ECC,Asserted\n" +
		"2,10/30/2026,10:01:00,Memory DIMM1,Uncorrectable ECC,Asserted\n" +
		"3,10/30/2026,10:02:00,Memory DIMM1,Uncorrectable ECC,Deasserted\n" +
		"4,10/30/2026,10:03:00,Power Supply,Presence detected,Asserted\n"
	events, partial := parseIPMISEL([]byte(data))
	if partial || len(events) != 4 {
		t.Fatalf("events=%v partial=%t", events, partial)
	}
	for i, kind := range []string{"corrected", "error", "recovery", "info"} {
		if events[i].Kind != kind || events[i].OccurredAt != nil {
			t.Fatalf("event %d: %+v", i, events[i])
		}
	}
	changed, _ := parseIPMISEL([]byte(strings.Replace(data, "10:00:00", "11:00:00", 1)))
	if changed[0].SourceRecordId == events[0].SourceRecordId {
		t.Fatal("SEL ID reuse lost a different event")
	}
}

func TestHealthCollectionReportsPartialCoverageAndDeduplicates(t *testing.T) {
	collector := &hostHealthCollector{journal: "/usr/bin/journalctl", ipmi: "/usr/bin/ipmitool", run: func(_ context.Context, command string, arguments ...string) ([]byte, error) {
		if command == "/usr/bin/journalctl" {
			return []byte(`{"__CURSOR":"same-record","PRIORITY":"4","MESSAGE":"corrected ECC"}`), nil
		}
		if strings.Contains(strings.Join(arguments, " "), "sensor list") {
			return []byte("CPU1 Temp,47.125,degrees C,ok,na,5,10,80,90,100\n"), nil
		}
		return nil, errors.New("test unavailable")
	}}
	report := collector.collect(context.Background())
	if len(report.Events) != 1 || len(report.Sensors) != 1 || report.Sources[2].Status != "unavailable" || report.ChassisPower != "unknown" {
		t.Fatalf("report=%+v", report)
	}
}

func TestHealthSELUsesBoundedBasicListing(t *testing.T) {
	queried := false
	collector := &hostHealthCollector{journalDisabled: true, ipmi: "/ipmitool", run: func(_ context.Context, _ string, arguments ...string) ([]byte, error) {
		if fmt.Sprint(arguments) == "[sel info]" {
			return []byte("Entries : 1\nFree Space : 100 bytes\nOverflow : false\n"), nil
		}
		if strings.Contains(strings.Join(arguments, " "), "sel") {
			queried = true
			if fmt.Sprint(arguments) != "[-c sel list last 128]" {
				t.Fatalf("unbounded or SDR-expanding SEL request: %v", arguments)
			}
			return []byte("1,10/30/2026,10:00:00,Memory #0x01,Correctable ECC,Asserted\n"), nil
		}
		return nil, errors.New("unavailable fixture")
	}}
	report := collector.collect(context.Background())
	if !queried || len(report.Events) != 1 || report.Events[0].Kind != "corrected" || report.Events[0].Component != "Memory #0x01" {
		t.Fatalf("basic SEL record lost: %+v", report)
	}
}

// Verbose field names follow ipmi_sel_print_std_entry_verbose; the values are
// from the independent local BMC probe, with its original clock left untrusted.
func TestSELFullLogFallsBackWithoutClaimingCompleteHistory(t *testing.T) {
	latest := "SEL Record ID : 0e37\n Record Type : 02\n Timestamp : 08/02/2026 08:54:22 AM CST\n Sensor Type : Memory\n Sensor Number : 5c\n Event Direction : Assertion Event\n Event Data : a00a20\n Description : Correctable ECC\n\nSensor Reading : unrelated current value\n"
	var calls []string
	collector := &hostHealthCollector{ipmi: "/ipmitool", run: func(_ context.Context, _ string, arguments ...string) ([]byte, error) {
		call := strings.Join(arguments, " ")
		calls = append(calls, call)
		switch call {
		case "sel info":
			return []byte("Entries : 3639\nFree Space : 0 bytes\nOverflow : true\n"), nil
		case "-c sel list last 128":
			return nil, context.DeadlineExceeded
		case "sel get 0xffff":
			return []byte(latest), nil
		default:
			t.Fatalf("unexpected command: %s", call)
			return nil, nil
		}
	}}
	events, source := collector.collectSEL(context.Background())
	if len(events) != 1 || events[0].Kind != "corrected" || events[0].OccurredAt != nil || strings.Contains(events[0].Message, "unrelated") || source.Status != "partial" || source.Limit != 1 || !strings.Contains(source.Message, "full or overflowed") || !strings.Contains(source.Message, "Earlier records are missing") || len(calls) != 3 {
		t.Fatalf("events=%v source=%v calls=%v", events, source, calls)
	}
	recovery := parseIPMILatestSEL([]byte(strings.Replace(latest, "Assertion Event", "Deassertion Event", 1)))
	if recovery == nil || recovery.Kind != "recovery" {
		t.Fatalf("deassertion=%v", recovery)
	}
	for _, invalid := range []string{"", strings.Replace(latest, "0e37", "ffff", 1), strings.Replace(latest, "Record Type : 02", "Record Type : c0", 1), strings.Replace(latest, "Assertion Event", "unknown", 1)} {
		if event := parseIPMILatestSEL([]byte(invalid)); event != nil {
			t.Fatalf("malformed or unsupported record accepted: %v", event)
		}
	}
}

func TestBMCIdentifyPolicyDeadlineAndCommandReplay(t *testing.T) {
	calls := 0
	collector := &hostHealthCollector{ipmi: "/ipmitool", run: func(_ context.Context, _ string, arguments ...string) ([]byte, error) {
		calls++
		if fmt.Sprint(arguments) != "[chassis identify 120]" {
			t.Fatalf("unexpected BMC command: %v", arguments)
		}
		return nil, nil
	}}
	command := &xirangv1.SetBMCIdentify{Enabled: true, ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second))}
	if ok, _ := collector.identifyCommand(context.Background(), command); ok || calls != 0 {
		t.Fatal("disabled BMC write was executed")
	}
	collector.identify = true
	expired := &xirangv1.SetBMCIdentify{Enabled: true, ExpiresAt: timestamppb.New(time.Now().Add(-time.Second))}
	if ok, _ := collector.identifyCommand(context.Background(), expired); ok || calls != 0 {
		t.Fatal("expired BMC write was executed")
	}
	directory := t.TempDir()
	message := &xirangv1.ControlMessage{MessageId: "locator-test", Payload: &xirangv1.ControlMessage_SetBmcIdentify{SetBmcIdentify: command}}
	_, ack, err := applyControlCommandWithHandlers(context.Background(), directory, commandState{Mode: "ready"}, message, commandHandlers{setBMCIdentify: collector.identifyCommand})
	if err != nil || !ack.Accepted {
		t.Fatalf("first request: %v %v", ack, err)
	}
	state, err := loadCommandState(directory)
	if err != nil {
		t.Fatal(err)
	}
	_, ack, err = applyControlCommandWithHandlers(context.Background(), directory, state, message, commandHandlers{setBMCIdentify: collector.identifyCommand})
	if err != nil || !ack.Accepted || calls != 1 {
		t.Fatalf("replay performed hardware I/O: calls=%d err=%v", calls, err)
	}
}
