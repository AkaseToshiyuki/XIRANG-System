//go:build linux

package agent

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

const (
	flowFamilyIPv4       = 4
	flowFamilyIPv6       = 6
	flowDirectionIngress = 1
	flowDirectionEgress  = 2
)

// flowKey is deliberately fixed-width and contains no process, socket, or
// payload data. Ports stay in network byte order so the BPF program can copy
// packet bytes directly without alignment-dependent conversions.
type flowKey struct {
	Family          uint8
	Protocol        uint8
	Direction       uint8
	_               uint8
	Source          [16]byte
	Destination     [16]byte
	SourcePort      [2]byte
	DestinationPort [2]byte
}

type flowValue struct {
	Packets uint64
	Bytes   uint64
}

type EBPFFlowCollector struct {
	config   FlowMonitoringConfig
	flows    *ebpf.Map
	ingress  *ebpf.Program
	egress   *ebpf.Program
	links    []link.Link
	previous map[flowKey]flowValue
	last     time.Time
	mu       sync.Mutex
}

func NewEBPFFlowCollector(config FlowMonitoringConfig) (*EBPFFlowCollector, error) {
	config.Enabled = true
	config, err := normalizeFlowMonitoringConfig(config)
	if err != nil {
		return nil, err
	}
	capability := ProbeEBPFCapability(config.CgroupPath)
	if !capability.CgroupV2 {
		return nil, errors.New("eBPF flow monitoring requires a cgroup v2 mount")
	}
	// Harmless on kernels which account BPF memory through memcg; required on
	// older supported kernels which still enforce RLIMIT_MEMLOCK.
	_ = rlimit.RemoveMemlock()
	flows, err := ebpf.NewMap(&ebpf.MapSpec{
		Name: "xirang_flows", Type: ebpf.LRUHash,
		KeySize: uint32(40), ValueSize: uint32(16), MaxEntries: config.MapEntries,
	})
	if err != nil {
		return nil, flowCollectorError("create flow map", err)
	}
	collector := &EBPFFlowCollector{config: config, flows: flows, previous: make(map[flowKey]flowValue), last: time.Now()}
	defer func() {
		if err != nil {
			_ = collector.Close()
		}
	}()
	collector.ingress, err = newFlowProgram(flows, ebpf.AttachCGroupInetIngress, flowDirectionIngress)
	if err != nil {
		return nil, flowCollectorError("load ingress flow program", err)
	}
	collector.egress, err = newFlowProgram(flows, ebpf.AttachCGroupInetEgress, flowDirectionEgress)
	if err != nil {
		return nil, flowCollectorError("load egress flow program", err)
	}
	for _, attachment := range []struct {
		attach  ebpf.AttachType
		program *ebpf.Program
	}{
		{ebpf.AttachCGroupInetIngress, collector.ingress},
		{ebpf.AttachCGroupInetEgress, collector.egress},
	} {
		var attached link.Link
		attached, err = link.AttachCgroup(link.CgroupOptions{
			Path: config.CgroupPath, Attach: attachment.attach, Program: attachment.program,
		})
		if err != nil {
			return nil, flowCollectorError("attach cgroup flow program", err)
		}
		collector.links = append(collector.links, attached)
	}
	return collector, nil
}

func (collector *EBPFFlowCollector) Sample() (FlowSample, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	now := time.Now()
	current := make(map[flowKey]flowValue)
	iterator := collector.flows.Iterate()
	var key flowKey
	var value flowValue
	for iterator.Next(&key, &value) {
		current[key] = value
	}
	if err := iterator.Err(); err != nil {
		return FlowSample{}, flowCollectorError("read flow map", err)
	}
	flows, truncated := sortedFlowDeltas(current, collector.previous, collector.config.ReportSize)
	sample := FlowSample{Flows: flows, Truncated: truncated, MapEntries: uint32(len(current)), Interval: now.Sub(collector.last)}
	collector.previous = current
	collector.last = now
	return sample, nil
}

func (collector *EBPFFlowCollector) Close() error {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	var closeErrors []error
	for index := len(collector.links) - 1; index >= 0; index-- {
		closeErrors = append(closeErrors, collector.links[index].Close())
	}
	collector.links = nil
	if collector.ingress != nil {
		closeErrors = append(closeErrors, collector.ingress.Close())
		collector.ingress = nil
	}
	if collector.egress != nil {
		closeErrors = append(closeErrors, collector.egress.Close())
		collector.egress = nil
	}
	if collector.flows != nil {
		closeErrors = append(closeErrors, collector.flows.Close())
		collector.flows = nil
	}
	return errors.Join(closeErrors...)
}

func newFlowProgram(flows *ebpf.Map, attach ebpf.AttachType, direction uint8) (*ebpf.Program, error) {
	instructions := flowInstructions(flows.FD(), direction)
	name := "xirang_ingress"
	if direction == flowDirectionEgress {
		name = "xirang_egress"
	}
	program, err := ebpf.NewProgramWithOptions(&ebpf.ProgramSpec{
		Name: name, Type: ebpf.CGroupSKB, AttachType: attach,
		Instructions: instructions, License: "GPL",
	}, ebpf.ProgramOptions{LogLevel: ebpf.LogLevelInstruction, LogSizeStart: 1 << 20})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return program, nil
}

// flowInstructions parses only direct IPv4/IPv6 TCP and UDP headers. IPv4
// options/fragments and IPv6 extension headers are skipped, keeping the verifier
// path small and predictable. Returning 1 preserves every packet.
func flowInstructions(mapFD int, direction uint8) asm.Instructions {
	const (
		keyOffset    = -40
		packetOffset = -104
		valueOffset  = -120
	)
	ins := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.LoadMem(asm.R8, asm.R6, 0, asm.Word),
		// The eBPF ISA has no 64-bit immediate-store encoding. Load zero into a
		// register once and use 64-bit register stores to initialize the key.
		asm.Mov.Imm(asm.R9, 0),
		asm.StoreMem(asm.RFP, keyOffset, asm.R9, asm.DWord),
		asm.StoreMem(asm.RFP, keyOffset+8, asm.R9, asm.DWord),
		asm.StoreMem(asm.RFP, keyOffset+16, asm.R9, asm.DWord),
		asm.StoreMem(asm.RFP, keyOffset+24, asm.R9, asm.DWord),
		asm.StoreMem(asm.RFP, keyOffset+32, asm.R9, asm.DWord),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Mov.Imm(asm.R2, 0),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, packetOffset),
		asm.Mov.Imm(asm.R4, 1),
		asm.FnSkbLoadBytes.Call(),
		asm.JNE.Imm(asm.R0, 0, "allow"),
		asm.LoadMem(asm.R7, asm.RFP, packetOffset, asm.Byte),
		asm.Mov.Reg(asm.R9, asm.R7),
		asm.And.Imm(asm.R9, 0xf0),
		asm.JEq.Imm(asm.R9, 0x40, "ipv4"),
		asm.JEq.Imm(asm.R9, 0x60, "ipv6"),
		asm.Ja.Label("allow"),

		asm.Mov.Reg(asm.R1, asm.R6).WithSymbol("ipv4"),
		asm.Mov.Imm(asm.R2, 0),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, packetOffset),
		asm.Mov.Imm(asm.R4, 24),
		asm.FnSkbLoadBytes.Call(),
		asm.JNE.Imm(asm.R0, 0, "allow"),
		asm.LoadMem(asm.R7, asm.RFP, packetOffset, asm.Byte),
		asm.Mov.Reg(asm.R9, asm.R7),
		asm.And.Imm(asm.R9, 0x0f),
		asm.JNE.Imm(asm.R9, 5, "allow"),
		asm.LoadMem(asm.R9, asm.RFP, packetOffset+6, asm.Half),
		asm.HostTo(asm.BE, asm.R9, asm.Half),
		asm.And.Imm(asm.R9, 0x3fff),
		asm.JNE.Imm(asm.R9, 0, "allow"),
		asm.LoadMem(asm.R7, asm.RFP, packetOffset+9, asm.Byte),
		asm.JEq.Imm(asm.R7, 6, "ipv4_copy"),
		asm.JNE.Imm(asm.R7, 17, "allow"),
		asm.StoreImm(asm.RFP, keyOffset, flowFamilyIPv4, asm.Byte).WithSymbol("ipv4_copy"),
		asm.StoreMem(asm.RFP, keyOffset+1, asm.R7, asm.Byte),
		asm.StoreImm(asm.RFP, keyOffset+2, int64(direction), asm.Byte),
		asm.LoadMem(asm.R9, asm.RFP, packetOffset+12, asm.Word),
		asm.StoreMem(asm.RFP, keyOffset+16, asm.R9, asm.Word),
		asm.LoadMem(asm.R9, asm.RFP, packetOffset+16, asm.Word),
		asm.StoreMem(asm.RFP, keyOffset+32, asm.R9, asm.Word),
		asm.LoadMem(asm.R9, asm.RFP, packetOffset+20, asm.Word),
		asm.StoreMem(asm.RFP, keyOffset+36, asm.R9, asm.Word),
		asm.Ja.Label("lookup"),

		asm.Mov.Reg(asm.R1, asm.R6).WithSymbol("ipv6"),
		asm.Mov.Imm(asm.R2, 0),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, packetOffset),
		asm.Mov.Imm(asm.R4, 44),
		asm.FnSkbLoadBytes.Call(),
		asm.JNE.Imm(asm.R0, 0, "allow"),
		asm.LoadMem(asm.R7, asm.RFP, packetOffset+6, asm.Byte),
		asm.JEq.Imm(asm.R7, 6, "ipv6_copy"),
		asm.JNE.Imm(asm.R7, 17, "allow"),
		asm.StoreImm(asm.RFP, keyOffset, flowFamilyIPv6, asm.Byte).WithSymbol("ipv6_copy"),
		asm.StoreMem(asm.RFP, keyOffset+1, asm.R7, asm.Byte),
		asm.StoreImm(asm.RFP, keyOffset+2, int64(direction), asm.Byte),
	}
	for source := int16(packetOffset + 8); source < packetOffset+44; source += 4 {
		destination := int16(keyOffset + 4 + int(source-(packetOffset+8)))
		if source >= packetOffset+24 {
			destination = int16(keyOffset + 20 + int(source-(packetOffset+24)))
		}
		if source >= packetOffset+40 {
			destination = int16(keyOffset + 36 + int(source-(packetOffset+40)))
		}
		ins = append(ins,
			asm.LoadMem(asm.R9, asm.RFP, source, asm.Word),
			asm.StoreMem(asm.RFP, destination, asm.R9, asm.Word),
		)
	}
	ins = append(ins,
		asm.LoadMapPtr(asm.R1, mapFD).WithSymbol("lookup"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, keyOffset),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "insert"),
		asm.Mov.Imm(asm.R1, 1),
		asm.StoreXAdd(asm.R0, asm.R1, asm.DWord),
		asm.Mov.Reg(asm.R9, asm.R0),
		asm.Add.Imm(asm.R9, 8),
		asm.StoreXAdd(asm.R9, asm.R8, asm.DWord).WithSymbol("add_bytes"),
		asm.Ja.Label("allow"),

		asm.Mov.Imm(asm.R9, 1).WithSymbol("insert"),
		asm.StoreMem(asm.RFP, valueOffset, asm.R9, asm.DWord),
		asm.StoreMem(asm.RFP, valueOffset+8, asm.R8, asm.DWord),
		asm.LoadMapPtr(asm.R1, mapFD),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, keyOffset),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, valueOffset),
		asm.Mov.Imm(asm.R4, 1),
		asm.FnMapUpdateElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "allow"),
		asm.LoadMapPtr(asm.R1, mapFD),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, keyOffset),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "allow"),
		asm.Mov.Imm(asm.R1, 1),
		asm.StoreXAdd(asm.R0, asm.R1, asm.DWord),
		asm.Mov.Reg(asm.R9, asm.R0),
		asm.Add.Imm(asm.R9, 8),
		asm.StoreXAdd(asm.R9, asm.R8, asm.DWord),
		asm.Ja.Label("allow"),

		asm.Mov.Imm(asm.R0, 1).WithSymbol("allow"),
		asm.Return(),
	)
	return ins
}
