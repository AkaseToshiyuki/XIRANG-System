package agent

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCollectTopologyParsesProcFiles(t *testing.T) {
	directory := t.TempDir()
	route4 := filepath.Join(directory, "route")
	route6 := filepath.Join(directory, "ipv6_route")
	arp := filepath.Join(directory, "arp")
	writeObservationFixture(t, route4, "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\neth0 00000000 0100000A 0003 0 0 100 00000000 0 0 0\neth0 0000000A 00000000 0001 0 0 0 00FFFFFF 0 0 0\n")
	writeObservationFixture(t, route6, "20010db8000000000000000000000000 40 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000064 00000000 00000000 00000001 eth0\n")
	writeObservationFixture(t, arp, "IP address HW type Flags HW address Mask Device\n10.0.0.1 0x1 0x2 00:11:22:33:44:55 * eth0\n10.0.0.9 0x1 0x0 00:00:00:00:00:00 * eth0\n")
	observation, err := collectTopology(context.Background(), route4, route6, arp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Routes) != 3 || observation.Routes[0].Destination != "0.0.0.0/0" || observation.Routes[0].Gateway != "10.0.0.1" {
		t.Fatalf("unexpected routes: %#v", observation.Routes)
	}
	if len(observation.Neighbors) != 1 || observation.Neighbors[0].Address != "10.0.0.1" {
		t.Fatalf("unexpected neighbors: %#v", observation.Neighbors)
	}
	if observation.Routes[2].Destination != "2001:db8::/64" || observation.Routes[2].Gateway != "fe80::1" {
		t.Fatalf("unexpected IPv6 route: %#v", observation.Routes[2])
	}
}

func TestParseNetlinkIPv6Neighbor(t *testing.T) {
	data := make([]byte, 12)
	data[0] = syscall.AF_INET6
	binary.NativeEndian.PutUint32(data[4:8], 7)
	binary.NativeEndian.PutUint16(data[8:10], 0x04)
	data[10] = 0x80
	destination := []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	data = append(data, netlinkAttribute(1, destination)...)
	data = append(data, netlinkAttribute(2, []byte{0, 17, 34, 51, 68, 85})...)
	neighbors, err := parseNetlinkNeighbors([]syscall.NetlinkMessage{{Header: syscall.NlMsghdr{Type: syscall.RTM_NEWNEIGH}, Data: data}}, func(index int) (string, error) {
		if index != 7 {
			t.Fatalf("unexpected interface index: %d", index)
		}
		return "eth0", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbors) != 1 || neighbors[0].Address != "fe80::1" || neighbors[0].MACAddress != "00:11:22:33:44:55" ||
		neighbors[0].State != "stale" || !neighbors[0].Router {
		t.Fatalf("unexpected netlink neighbors: %#v", neighbors)
	}
}

func TestParseIPRouteJSONIncludesPolicyTables(t *testing.T) {
	routes, err := parseIPRouteJSON([]byte(`[
		{"dst":"198.51.100.65","dev":"tun0","table":"52","flags":[]},
		{"dst":"192.168.6.0/24","dev":"eth0","protocol":"kernel","metric":100,"flags":[]},
		{"dst":"default","gateway":"192.168.6.1","dev":"eth0","table":"main","metric":200,"flags":[]},
		{"type":"local","dst":"192.168.6.222","dev":"eth0","table":"local","flags":[]}
	]`), "ipv4")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("unexpected routes: %#v", routes)
	}
	if routes[0].Destination != "198.51.100.65/32" || routes[0].Table != 52 || routes[0].InterfaceName != "tun0" {
		t.Fatalf("policy route was not normalized: %#v", routes[0])
	}
	if routes[2].Destination != "0.0.0.0/0" || routes[2].Gateway != "192.168.6.1" || routes[2].Table != 254 || routes[2].Metric != 200 {
		t.Fatalf("default route was not normalized: %#v", routes[2])
	}
}

func TestParseIPRouteJSONNormalizesIPv6HostRoute(t *testing.T) {
	routes, err := parseIPRouteJSON([]byte(`[{"dst":"2001:db8::2","dev":"wg0","table":123}]`), "ipv6")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Destination != "2001:db8::2/128" || routes[0].Table != 123 {
		t.Fatalf("unexpected IPv6 route: %#v", routes)
	}
}

func netlinkAttribute(kind uint16, value []byte) []byte {
	length := 4 + len(value)
	result := make([]byte, (length+3)&^3)
	binary.NativeEndian.PutUint16(result[0:2], uint16(length))
	binary.NativeEndian.PutUint16(result[2:4], kind)
	copy(result[4:], value)
	return result
}

func TestParseLLDPJSON(t *testing.T) {
	neighbors, err := parseLLDPJSON([]byte(`{"lldp":{"interface":{"eth0":{"chassis":{"switch-a":{"id":{"type":"mac","value":"00:aa:bb:cc:dd:ee"},"name":"leaf-a","descr":"switch","mgmt-ip":{"10.0.0.2":{"if":"2"}}}},"port":{"id":{"type":"ifname","value":"Ethernet1"}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbors) != 1 || neighbors[0].ChassisID != "00:aa:bb:cc:dd:ee" || neighbors[0].PortID != "Ethernet1" ||
		neighbors[0].SystemName != "leaf-a" || len(neighbors[0].ManagementAddresses) != 1 {
		t.Fatalf("unexpected LLDP neighbors: %#v", neighbors)
	}
}

func TestCollectTopologyIgnoresMissingOptionalFiles(t *testing.T) {
	directory := t.TempDir()
	route4 := filepath.Join(directory, "route")
	writeObservationFixture(t, route4, "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n")
	observation, err := collectTopology(context.Background(), route4, filepath.Join(directory, "missing-v6"), filepath.Join(directory, "missing-arp"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Routes) != 0 || len(observation.Neighbors) != 0 {
		t.Fatalf("unexpected observation: %#v", observation)
	}
}

func TestBoundedLLDPOutput(t *testing.T) {
	var buffer boundedBuffer
	if _, err := buffer.Write(make([]byte, maxLLDPOutput)); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte{1}); err == nil {
		t.Fatal("expected oversized LLDP output to fail")
	}
}
