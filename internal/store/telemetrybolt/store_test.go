package telemetrybolt_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	telemetrybolt "github.com/xirang-grid/xirang/internal/store/telemetrybolt"
	"github.com/xirang-grid/xirang/internal/telemetry"
)

func TestTelemetryStorePersistsInventoryAndBoundsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry.db")
	store, err := telemetrybolt.Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	service := telemetry.NewService(store)
	base := time.Now().UTC().Add(-time.Minute)
	if err := service.ReportInventory(context.Background(), "node-1", base, &xirangv1.InventorySnapshot{
		Hostname: "compute-a", Architecture: "arm64", LogicalCpuCount: 12, MemoryBytes: 32 << 30,
		NetworkInterfaces: []*xirangv1.NetworkInterface{{
			Name: "eth0", MacAddress: "00:11:22:33:44:55", Addresses: []string{"10.0.0.2/24"},
			SpeedBitsPerSecond: 10_000_000_000, Mtu: 1500,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 3; index++ {
		if err := service.ReportObservation(context.Background(), "node-1", "message-"+string(rune('0'+index)), base.Add(time.Duration(index)*time.Second), &xirangv1.ResourceObservation{
			UsedMilliCpu: int64(index * 1000), UsedMemoryBytes: uint64(index) << 30,
			ReceivedBytes: uint64(index * 100), TransmittedBytes: uint64(index * 200),
			InterfaceCounters: []*xirangv1.NetworkInterfaceCounters{{
				Name: "eth0", ReceivedBytes: uint64(index * 90), TransmittedBytes: uint64(index * 180),
				ReceivedPackets: uint64(index * 9), TransmittedPackets: uint64(index * 18),
				ReceiveErrors: uint64(index), TransmitErrors: uint64(index * 2),
				ReceiveDropped: uint64(index * 3), TransmitDropped: uint64(index * 4),
				PacketCountersSupported: true,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := telemetrybolt.Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	inventory, err := reopened.GetInventory(context.Background(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Architecture != "arm64" || len(inventory.NetworkInterfaces) != 1 || inventory.NetworkInterfaces[0].MTU != 1500 {
		t.Fatalf("unexpected inventory: %#v", inventory)
	}
	observations, err := reopened.ListObservations(context.Background(), "node-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[0].UsedMilliCPU != 3000 || observations[1].UsedMilliCPU != 2000 {
		t.Fatalf("retention or ordering failed: %#v", observations)
	}
	if observations[0].InterfaceCounters["eth0"].ReceivedBytes != 270 {
		t.Fatalf("interface counters were not persisted: %#v", observations[0].InterfaceCounters)
	}
	if observations[0].InterfaceCounters["eth0"].TransmitDropped != 12 || observations[0].InterfaceCounters["eth0"].ReceiveErrors != 3 {
		t.Fatalf("interface health counters were not persisted: %#v", observations[0].InterfaceCounters)
	}
	if !observations[0].InterfaceCounters["eth0"].PacketCountersSupported {
		t.Fatal("packet counter support marker was not persisted")
	}
}
