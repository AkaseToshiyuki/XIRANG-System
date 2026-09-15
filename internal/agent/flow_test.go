package agent

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestNormalizeFlowMonitoringConfig(t *testing.T) {
	config, err := normalizeFlowMonitoringConfig(FlowMonitoringConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if config.CgroupPath != defaultFlowCgroupPath || config.MapEntries != defaultFlowMapEntries || config.ReportSize != defaultFlowReportSize || config.Interval != 10*time.Second {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	if _, err := normalizeFlowMonitoringConfig(FlowMonitoringConfig{MapEntries: 1}); err == nil {
		t.Fatal("expected a bounded map-size error")
	}
}

func TestSortedFlowDeltas(t *testing.T) {
	key := flowKey{Family: flowFamilyIPv4, Protocol: 6, Direction: flowDirectionEgress, SourcePort: [2]byte{0x1f, 0x90}, DestinationPort: [2]byte{0, 80}}
	copy(key.Source[12:], []byte{10, 0, 0, 1})
	copy(key.Destination[12:], []byte{10, 0, 0, 2})
	current := map[flowKey]flowValue{key: {Packets: 7, Bytes: 700}}
	previous := map[flowKey]flowValue{key: {Packets: 2, Bytes: 200}}
	flows, truncated := sortedFlowDeltas(current, previous, 10)
	if truncated || len(flows) != 1 {
		t.Fatalf("unexpected result: %#v truncated=%t", flows, truncated)
	}
	flow := flows[0]
	if flow.SourceAddress != "10.0.0.1" || flow.DestinationAddress != "10.0.0.2" || flow.SourcePort != 8080 || flow.DestinationPort != 80 || flow.Packets != 5 || flow.Bytes != 500 {
		t.Fatalf("unexpected flow: %#v", flow)
	}
}

func TestFlowInstructionsMarshalForBothDirections(t *testing.T) {
	for _, direction := range []uint8{flowDirectionIngress, flowDirectionEgress} {
		var buffer bytes.Buffer
		if err := flowInstructions(123, direction).Marshal(&buffer, binary.LittleEndian); err != nil {
			t.Fatalf("marshal direction %d: %v", direction, err)
		}
		if buffer.Len() == 0 {
			t.Fatal("expected encoded instructions")
		}
	}
}
