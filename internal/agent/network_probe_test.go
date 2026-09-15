package agent

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollectNetworkPathProbesTraversesAddressProduct(t *testing.T) {
	locals := []NetworkInterfaceInventory{
		{Name: "eth0", Addresses: []string{"192.0.2.10/24", "2001:db8::10/64"}, SpeedBitsPerSecond: 10_000_000_000},
		{Name: "eth1", Addresses: []string{"198.51.100.10/24"}, SpeedBitsPerSecond: 25_000_000_000},
	}
	targets := []NetworkProbeTarget{
		{NodeID: "node-a", InterfaceName: "eno1", Address: "192.0.2.20", SpeedBitsPerSecond: 1_000_000_000},
		{NodeID: "node-a", InterfaceName: "eno2", Address: "198.51.100.20", SpeedBitsPerSecond: 25_000_000_000},
		{NodeID: "node-b", InterfaceName: "eth0", Address: "2001:db8::20", SpeedBitsPerSecond: 10_000_000_000},
	}
	var calls atomic.Int32
	results := collectNetworkPathProbes(context.Background(), locals, targets, func(_ context.Context, source, target netip.Addr) (time.Duration, error) {
		calls.Add(1)
		if source.String() == "198.51.100.10" && target.String() == "198.51.100.20" {
			return 250 * time.Microsecond, nil
		}
		return 0, errors.New("not reachable")
	})
	if calls.Load() != 15 || len(results) != 5 {
		t.Fatalf("expected all same-family address pairs, calls=%d results=%d", calls.Load(), len(results))
	}
	var selected PathProbeObservation
	for _, result := range results {
		if result.Reachable {
			selected = result
		}
	}
	if selected.TargetNodeID != "node-a" || selected.SourceInterface != "eth1" ||
		selected.TargetInterface != "eno2" || selected.LatencyMicros != 250 {
		t.Fatalf("unexpected reachable result: %#v", selected)
	}
}

func TestProbeAddressRejectsNonRoutableAddresses(t *testing.T) {
	for _, value := range []string{"127.0.0.1/8", "::1/128", "fe80::1/64", "0.0.0.0"} {
		if _, ok := probeAddress(value); ok {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestParsePingLatency(t *testing.T) {
	match := pingTimePattern.FindStringSubmatch("64 bytes: time=0.345 ms")
	if len(match) != 3 || match[2] != "0.345" {
		t.Fatalf("unexpected match: %#v", match)
	}
}
