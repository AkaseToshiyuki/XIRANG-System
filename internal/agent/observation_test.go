package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResourceSamplerReadsProcCounters(t *testing.T) {
	directory := t.TempDir()
	stat := filepath.Join(directory, "stat")
	meminfo := filepath.Join(directory, "meminfo")
	netdev := filepath.Join(directory, "netdev")
	writeObservationFixture(t, stat, "cpu 100 0 50 800 50 0 0 0\n")
	writeObservationFixture(t, meminfo, "MemTotal: 1000 kB\nMemAvailable: 400 kB\n")
	writeObservationFixture(t, netdev, "Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n  eth0: 100 10 2 3 0 0 0 0 200 20 4 5 0 0 0 0\n    lo: 25 2 0 0 0 0 0 0 50 4 0 0 0 0 0 0\n")
	sampler, err := newResourceSampler(stat, meminfo, netdev, 4)
	if err != nil {
		t.Fatal(err)
	}
	writeObservationFixture(t, stat, "cpu 150 0 100 850 50 0 0 0\n")
	observation, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if observation.UsedMilliCPU != 2666 || observation.UsedMemoryBytes != 600*1024 ||
		observation.ReceivedBytes != 125 || observation.TransmittedBytes != 250 || len(observation.InterfaceCounters) != 2 ||
		observation.InterfaceCounters[0].Name != "eth0" || observation.InterfaceCounters[0].ReceivedBytes != 100 ||
		observation.InterfaceCounters[0].ReceivedPackets != 10 || observation.InterfaceCounters[0].TransmittedPackets != 20 ||
		observation.InterfaceCounters[0].ReceiveErrors != 2 || observation.InterfaceCounters[0].TransmitErrors != 4 ||
		observation.InterfaceCounters[0].ReceiveDropped != 3 || observation.InterfaceCounters[0].TransmitDropped != 5 ||
		!observation.InterfaceCounters[0].PacketCountersSupported {
		t.Fatalf("unexpected resource observation: %#v", observation)
	}
}

func TestInterfaceSpeedBits(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "eth0"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeObservationFixture(t, filepath.Join(directory, "eth0", "speed"), "10000\n")
	if speed := interfaceSpeedBits(directory, "eth0"); speed != 10_000_000_000 {
		t.Fatalf("unexpected interface speed: %d", speed)
	}
	if speed := interfaceSpeedBits(directory, "../escape"); speed != 0 {
		t.Fatalf("unsafe interface name was accepted: %d", speed)
	}
}

func writeObservationFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
