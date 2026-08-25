package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxLLDPOutput  = 1 << 20
	maxRouteOutput = 4 << 20
)

type TopologyObservation struct {
	Routes        []RouteObservation        `json:"routes,omitempty"`
	Neighbors     []NeighborObservation     `json:"neighbors,omitempty"`
	LLDPNeighbors []LLDPNeighborObservation `json:"lldpNeighbors,omitempty"`
	PathProbes    []PathProbeObservation    `json:"pathProbes,omitempty"`
}

type RouteObservation struct {
	Destination   string `json:"destination"`
	Gateway       string `json:"gateway,omitempty"`
	InterfaceName string `json:"interfaceName"`
	Metric        uint32 `json:"metric,omitempty"`
	Table         uint32 `json:"table"`
	Family        string `json:"family"`
}

type NeighborObservation struct {
	Address       string `json:"address"`
	MACAddress    string `json:"macAddress,omitempty"`
	InterfaceName string `json:"interfaceName"`
	State         string `json:"state"`
	Router        bool   `json:"router,omitempty"`
}

type LLDPNeighborObservation struct {
	LocalInterface      string   `json:"localInterface"`
	ChassisID           string   `json:"chassisId"`
	PortID              string   `json:"portId,omitempty"`
	SystemName          string   `json:"systemName,omitempty"`
	SystemDescription   string   `json:"systemDescription,omitempty"`
	ManagementAddresses []string `json:"managementAddresses,omitempty"`
}

func CollectTopology(ctx context.Context) (TopologyObservation, error) {
	return CollectTopologyWithProbes(ctx, nil, nil)
}

func CollectTopologyWithProbes(ctx context.Context, localInterfaces []NetworkInterfaceInventory, targets []NetworkProbeTarget) (TopologyObservation, error) {
	observation, err := collectTopology(ctx, "/proc/net/route", "/proc/net/ipv6_route", "/proc/net/arp", []string{
		"/usr/sbin/lldpctl", "/usr/bin/lldpctl", "/sbin/lldpctl", "/bin/lldpctl",
	})
	if err != nil {
		return TopologyObservation{}, err
	}
	if neighbors, err := readKernelNeighbors(); err == nil {
		observation.Neighbors = neighbors
		sort.Slice(observation.Neighbors, func(left, right int) bool {
			return neighborSortKey(observation.Neighbors[left]) < neighborSortKey(observation.Neighbors[right])
		})
	}
	if routes, err := collectKernelRoutes(ctx); err == nil {
		observation.Routes = routes
	}
	if len(localInterfaces) > 0 && len(targets) > 0 {
		observation.PathProbes = CollectNetworkPathProbes(ctx, localInterfaces, targets)
	}
	return observation, nil
}

// collectKernelRoutes reads every Linux policy-routing table through iproute2.
// The proc files used by collectTopology only expose the main table, so they
// remain a portability fallback when iproute2 or its JSON output is unavailable.
func collectKernelRoutes(parent context.Context) ([]RouteObservation, error) {
	executable, err := exec.LookPath("ip")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var routes []RouteObservation
	for _, family := range []struct {
		flag string
		name string
	}{
		{"-4", "ipv4"},
		{"-6", "ipv6"},
	} {
		var output limitedBuffer
		output.limit = maxRouteOutput
		command := exec.CommandContext(ctx, executable, "-json", family.flag, "route", "show", "table", "all")
		command.Stdout = &output
		if err := command.Run(); err != nil {
			return nil, err
		}
		parsed, err := parseIPRouteJSON(output.Bytes(), family.name)
		if err != nil {
			return nil, err
		}
		routes = append(routes, parsed...)
	}
	routes = uniqueRoutes(routes)
	sort.Slice(routes, func(left, right int) bool {
		return routeSortKey(routes[left]) < routeSortKey(routes[right])
	})
	return routes, nil
}

func parseIPRouteJSON(data []byte, family string) ([]RouteObservation, error) {
	var document []struct {
		Type        string      `json:"type"`
		Destination string      `json:"dst"`
		Gateway     string      `json:"gateway"`
		Device      string      `json:"dev"`
		Metric      json.Number `json:"metric"`
		Table       any         `json:"table"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	routes := make([]RouteObservation, 0, len(document))
	for _, item := range document {
		if item.Device == "" || (item.Type != "" && item.Type != "unicast") {
			continue
		}
		destination, err := normalizeRouteDestination(item.Destination, family)
		if err != nil {
			continue
		}
		gateway := ""
		if item.Gateway != "" {
			address, err := netip.ParseAddr(item.Gateway)
			if err != nil {
				continue
			}
			gateway = address.Unmap().String()
		}
		metric, err := parseRouteUint(item.Metric, 0)
		if err != nil {
			continue
		}
		table, err := parseRouteTable(item.Table)
		if err != nil {
			continue
		}
		routes = append(routes, RouteObservation{
			Destination: destination, Gateway: gateway, InterfaceName: item.Device,
			Metric: metric, Table: table, Family: family,
		})
	}
	return routes, nil
}

func normalizeRouteDestination(value, family string) (string, error) {
	if value == "" || value == "default" {
		if family == "ipv4" {
			return "0.0.0.0/0", nil
		}
		return "::/0", nil
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Masked().String(), nil
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return "", err
	}
	address = address.Unmap()
	if (family == "ipv4") != address.Is4() {
		return "", errors.New("route address family does not match")
	}
	bits := 128
	if address.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(address, bits).String(), nil
}

func parseRouteUint(value json.Number, fallback uint32) (uint32, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value.String(), 10, 32)
	return uint32(parsed), err
}

func parseRouteTable(value any) (uint32, error) {
	if value == nil {
		return 254, nil
	}
	switch typed := value.(type) {
	case json.Number:
		return parseRouteUint(typed, 254)
	case string:
		if named, ok := map[string]uint32{"unspec": 0, "default": 253, "main": 254, "local": 255}[typed]; ok {
			return named, nil
		}
		parsed, err := strconv.ParseUint(typed, 10, 32)
		return uint32(parsed), err
	default:
		return 0, errors.New("route table is invalid")
	}
}

func uniqueRoutes(routes []RouteObservation) []RouteObservation {
	seen := make(map[string]struct{}, len(routes))
	result := make([]RouteObservation, 0, len(routes))
	for _, route := range routes {
		key := routeSortKey(route)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, route)
	}
	return result
}

func collectTopology(ctx context.Context, route4Path, route6Path, arpPath string, lldpCandidates []string) (TopologyObservation, error) {
	routes4, err := readIPv4Routes(route4Path)
	if err != nil {
		return TopologyObservation{}, err
	}
	routes6, err := readIPv6Routes(route6Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return TopologyObservation{}, err
	}
	neighbors, err := readARPNeighbors(arpPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return TopologyObservation{}, err
	}
	lldpNeighbors := collectLLDPNeighbors(ctx, lldpCandidates)
	routes := append(routes4, routes6...)
	sort.Slice(routes, func(left, right int) bool {
		return routeSortKey(routes[left]) < routeSortKey(routes[right])
	})
	sort.Slice(neighbors, func(left, right int) bool {
		return neighborSortKey(neighbors[left]) < neighborSortKey(neighbors[right])
	})
	sort.Slice(lldpNeighbors, func(left, right int) bool {
		return lldpSortKey(lldpNeighbors[left]) < lldpSortKey(lldpNeighbors[right])
	})
	return TopologyObservation{Routes: routes, Neighbors: neighbors, LLDPNeighbors: lldpNeighbors}, nil
}

func readIPv4Routes(path string) ([]RouteObservation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var routes []RouteObservation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 11 || fields[0] == "Iface" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil {
			return nil, errors.New("IPv4 route flags are invalid")
		}
		if flags&0x1 == 0 || flags&0x200 != 0 {
			continue
		}
		destination, err := parseProcIPv4(fields[1])
		if err != nil {
			return nil, fmt.Errorf("parse IPv4 route destination: %w", err)
		}
		gateway, err := parseProcIPv4(fields[2])
		if err != nil {
			return nil, fmt.Errorf("parse IPv4 route gateway: %w", err)
		}
		maskIP, err := parseProcIPv4(fields[7])
		if err != nil {
			return nil, fmt.Errorf("parse IPv4 route mask: %w", err)
		}
		ones, bits := net.IPMask(maskIP.AsSlice()).Size()
		if bits != 32 || ones < 0 {
			return nil, errors.New("IPv4 route mask is not contiguous")
		}
		metric, err := strconv.ParseUint(fields[6], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse IPv4 route metric: %w", err)
		}
		route := RouteObservation{
			Destination: netip.PrefixFrom(destination, ones).Masked().String(), InterfaceName: fields[0],
			Metric: uint32(metric), Table: 254, Family: "ipv4",
		}
		if !gateway.IsUnspecified() {
			route.Gateway = gateway.String()
		}
		routes = append(routes, route)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return routes, nil
}

func readIPv6Routes(path string) ([]RouteObservation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var routes []RouteObservation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			return nil, errors.New("IPv6 route row is incomplete")
		}
		sourcePrefixLength, err := strconv.ParseUint(fields[3], 16, 8)
		if err != nil {
			return nil, errors.New("IPv6 source prefix length is invalid")
		}
		flags, err := strconv.ParseUint(fields[8], 16, 32)
		if err != nil {
			return nil, errors.New("IPv6 route flags are invalid")
		}
		if sourcePrefixLength != 0 || flags&(0x200|0x01000000|0x80000000) != 0 {
			continue
		}
		destination, err := parseProcIPv6(fields[0])
		if err != nil {
			return nil, fmt.Errorf("parse IPv6 route destination: %w", err)
		}
		prefixLength, err := strconv.ParseUint(fields[1], 16, 8)
		if err != nil || prefixLength > 128 {
			return nil, errors.New("IPv6 route prefix length is invalid")
		}
		gateway, err := parseProcIPv6(fields[4])
		if err != nil {
			return nil, fmt.Errorf("parse IPv6 route gateway: %w", err)
		}
		metric, err := strconv.ParseUint(fields[5], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("parse IPv6 route metric: %w", err)
		}
		route := RouteObservation{
			Destination: netip.PrefixFrom(destination, int(prefixLength)).Masked().String(), InterfaceName: fields[9],
			Metric: uint32(metric), Table: 254, Family: "ipv6",
		}
		if !gateway.IsUnspecified() {
			route.Gateway = gateway.String()
		}
		routes = append(routes, route)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return routes, nil
}

func readARPNeighbors(path string) ([]NeighborObservation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var neighbors []NeighborObservation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || fields[0] == "IP" {
			continue
		}
		address, err := netip.ParseAddr(fields[0])
		if err != nil || !address.Is4() {
			return nil, errors.New("ARP neighbor address is invalid")
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(fields[2], "0x"), 16, 32)
		if err != nil {
			return nil, errors.New("ARP neighbor flags are invalid")
		}
		if flags&0x2 == 0 {
			continue
		}
		if _, err := net.ParseMAC(fields[3]); err != nil {
			return nil, errors.New("ARP neighbor MAC address is invalid")
		}
		state := "reachable"
		if flags&0x4 != 0 {
			state = "permanent"
		}
		neighbors = append(neighbors, NeighborObservation{
			Address: address.String(), MACAddress: strings.ToLower(fields[3]), InterfaceName: fields[5], State: state,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return neighbors, nil
}

func readKernelNeighbors() ([]NeighborObservation, error) {
	raw, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	messages, err := syscall.ParseNetlinkMessage(raw)
	if err != nil {
		return nil, err
	}
	return parseNetlinkNeighbors(messages, func(index int) (string, error) {
		networkInterface, err := net.InterfaceByIndex(index)
		if err != nil {
			return "", err
		}
		return networkInterface.Name, nil
	})
}

func parseNetlinkNeighbors(messages []syscall.NetlinkMessage, interfaceName func(int) (string, error)) ([]NeighborObservation, error) {
	const (
		ndMessageLength = 12
		ndaDestination  = 1
		ndaLinkAddress  = 2
		neighborRouter  = 0x80
	)
	var neighbors []NeighborObservation
	for _, message := range messages {
		if message.Header.Type != syscall.RTM_NEWNEIGH || len(message.Data) < ndMessageLength {
			continue
		}
		family := int(message.Data[0])
		if family != syscall.AF_INET && family != syscall.AF_INET6 {
			continue
		}
		index := int(int32(binary.NativeEndian.Uint32(message.Data[4:8])))
		stateBits := binary.NativeEndian.Uint16(message.Data[8:10])
		state := neighborState(stateBits)
		if state == "" || state == "incomplete" || state == "failed" || state == "noarp" {
			continue
		}
		name, err := interfaceName(index)
		if err != nil || name == "" {
			continue
		}
		attributes, err := parseNeighborAttributes(message.Data[ndMessageLength:])
		if err != nil {
			return nil, err
		}
		destination := attributes[ndaDestination]
		if (family == syscall.AF_INET && len(destination) != net.IPv4len) || (family == syscall.AF_INET6 && len(destination) != net.IPv6len) {
			continue
		}
		address, ok := netip.AddrFromSlice(destination)
		if !ok {
			continue
		}
		mac := ""
		if linkAddress := attributes[ndaLinkAddress]; len(linkAddress) > 0 {
			mac = strings.ToLower(net.HardwareAddr(linkAddress).String())
		}
		neighbors = append(neighbors, NeighborObservation{
			Address: address.Unmap().String(), MACAddress: mac, InterfaceName: name,
			State: state, Router: message.Data[10]&neighborRouter != 0,
		})
	}
	return neighbors, nil
}

func parseNeighborAttributes(data []byte) (map[uint16][]byte, error) {
	attributes := map[uint16][]byte{}
	for len(data) >= 4 {
		length := int(binary.NativeEndian.Uint16(data[0:2]))
		kind := binary.NativeEndian.Uint16(data[2:4])
		if length < 4 || length > len(data) {
			return nil, errors.New("netlink neighbor attribute is malformed")
		}
		attributes[kind] = append([]byte(nil), data[4:length]...)
		aligned := (length + 3) &^ 3
		if aligned > len(data) {
			return nil, errors.New("netlink neighbor attribute alignment is malformed")
		}
		data = data[aligned:]
	}
	if len(data) != 0 {
		return nil, errors.New("netlink neighbor attributes are truncated")
	}
	return attributes, nil
}

func neighborState(state uint16) string {
	for _, candidate := range []struct {
		mask uint16
		name string
	}{
		{0x80, "permanent"}, {0x40, "noarp"}, {0x02, "reachable"}, {0x04, "stale"},
		{0x08, "delay"}, {0x10, "probe"}, {0x01, "incomplete"}, {0x20, "failed"},
	} {
		if state&candidate.mask != 0 {
			return candidate.name
		}
	}
	return ""
}

func parseProcIPv4(value string) (netip.Addr, error) {
	encoded, err := hex.DecodeString(value)
	if err != nil || len(encoded) != 4 {
		return netip.Addr{}, errors.New("expected eight hexadecimal digits")
	}
	return netip.AddrFrom4([4]byte{encoded[3], encoded[2], encoded[1], encoded[0]}), nil
}

func parseProcIPv6(value string) (netip.Addr, error) {
	encoded, err := hex.DecodeString(value)
	if err != nil || len(encoded) != 16 {
		return netip.Addr{}, errors.New("expected 32 hexadecimal digits")
	}
	var address [16]byte
	copy(address[:], encoded)
	return netip.AddrFrom16(address), nil
}

func collectLLDPNeighbors(parent context.Context, candidates []string) []LLDPNeighborObservation {
	var executable string
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			executable = candidate
			break
		}
	}
	if executable == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var output boundedBuffer
	command := exec.CommandContext(ctx, executable, "-f", "json", "show", "neighbors", "details")
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil
	}
	neighbors, err := parseLLDPJSON(output.Bytes())
	if err != nil {
		return nil
	}
	return neighbors
}

func parseLLDPJSON(data []byte) ([]LLDPNeighborObservation, error) {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	lldp, ok := document["lldp"].(map[string]any)
	if !ok {
		return nil, errors.New("LLDP document is missing lldp")
	}
	interfaces, ok := lldp["interface"].(map[string]any)
	if !ok {
		return nil, errors.New("LLDP document is missing interfaces")
	}
	neighbors := make([]LLDPNeighborObservation, 0, len(interfaces))
	for localInterface, raw := range interfaces {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		chassis := unwrapNamedObject(entry["chassis"])
		port := unwrapNamedObject(entry["port"])
		neighbor := LLDPNeighborObservation{
			LocalInterface: localInterface, ChassisID: nestedString(chassis, "id", "value"),
			PortID: nestedString(port, "id", "value"), SystemName: directString(chassis, "name"),
			SystemDescription: directString(chassis, "descr"), ManagementAddresses: managementAddresses(chassis),
		}
		if neighbor.ChassisID == "" {
			continue
		}
		sort.Strings(neighbor.ManagementAddresses)
		neighbors = append(neighbors, neighbor)
	}
	return neighbors, nil
}

func unwrapNamedObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	if object == nil {
		return nil
	}
	if _, ok := object["id"]; ok {
		return object
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if nested, ok := object[key].(map[string]any); ok {
			return nested
		}
	}
	return object
}

func directString(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func nestedString(object map[string]any, parent, child string) string {
	nested, _ := object[parent].(map[string]any)
	return directString(nested, child)
}

func managementAddresses(chassis map[string]any) []string {
	var result []string
	for _, key := range []string{"mgmt-ip", "management-address", "management_addresses"} {
		value, ok := chassis[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if _, err := netip.ParseAddr(typed); err == nil {
				result = append(result, typed)
			}
		case map[string]any:
			for address := range typed {
				if _, err := netip.ParseAddr(address); err == nil {
					result = append(result, address)
				}
			}
		}
	}
	return result
}

func topologyFingerprint(observation TopologyObservation) [32]byte {
	encoded, _ := json.Marshal(observation)
	return sha256.Sum256(encoded)
}

func routeSortKey(route RouteObservation) string {
	return strings.Join([]string{route.Family, route.Destination, route.Gateway, route.InterfaceName, fmt.Sprint(route.Table), fmt.Sprint(route.Metric)}, "\x00")
}

func neighborSortKey(neighbor NeighborObservation) string {
	return strings.Join([]string{neighbor.InterfaceName, neighbor.Address, neighbor.MACAddress, neighbor.State}, "\x00")
}

func lldpSortKey(neighbor LLDPNeighborObservation) string {
	return strings.Join([]string{neighbor.LocalInterface, neighbor.ChassisID, neighbor.PortID}, "\x00")
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > buffer.limit {
		return 0, errors.New("command output exceeds limit")
	}
	return buffer.Buffer.Write(data)
}

type boundedBuffer struct{ limitedBuffer }

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	buffer.limit = maxLLDPOutput
	return buffer.limitedBuffer.Write(data)
}
