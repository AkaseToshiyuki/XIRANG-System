package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxNetworkProbePairs = 4096
	networkProbeWorkers  = 8
	networkProbeTimeout  = 1500 * time.Millisecond
)

var pingTimePattern = regexp.MustCompile(`time([=<])([0-9]+(?:\.[0-9]+)?)\s*ms`)

type NetworkProbeTarget struct {
	NodeID             string
	InterfaceName      string
	Address            string
	SpeedBitsPerSecond uint64
}

type PathProbeObservation struct {
	TargetNodeID             string `json:"targetNodeId"`
	SourceInterface          string `json:"sourceInterface"`
	SourceAddress            string `json:"sourceAddress"`
	TargetInterface          string `json:"targetInterface"`
	TargetAddress            string `json:"targetAddress"`
	SourceSpeedBitsPerSecond uint64 `json:"sourceSpeedBitsPerSecond,omitempty"`
	TargetSpeedBitsPerSecond uint64 `json:"targetSpeedBitsPerSecond,omitempty"`
	LatencyMicros            int64  `json:"latencyMicros,omitempty"`
	Reachable                bool   `json:"reachable"`
	Failure                  string `json:"failure,omitempty"`
}

type networkProbePair struct {
	source NetworkInterfaceInventory
	local  netip.Addr
	target NetworkProbeTarget
	remote netip.Addr
}

type pingProbe func(context.Context, netip.Addr, netip.Addr) (time.Duration, error)

func CollectNetworkPathProbes(ctx context.Context, localInterfaces []NetworkInterfaceInventory, targets []NetworkProbeTarget) []PathProbeObservation {
	return collectNetworkPathProbes(ctx, localInterfaces, targets, runPingProbe)
}

func collectNetworkPathProbes(ctx context.Context, localInterfaces []NetworkInterfaceInventory, targets []NetworkProbeTarget, probe pingProbe) []PathProbeObservation {
	pairs := make([]networkProbePair, 0)
	seen := make(map[string]struct{})
	for _, localInterface := range localInterfaces {
		for _, rawLocal := range localInterface.Addresses {
			local, ok := probeAddress(rawLocal)
			if !ok {
				continue
			}
			for _, target := range targets {
				remote, ok := probeAddress(target.Address)
				if !ok || local.BitLen() != remote.BitLen() {
					continue
				}
				key := strings.Join([]string{
					localInterface.Name, local.String(), target.NodeID, target.InterfaceName, remote.String(),
				}, "\x00")
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				pairs = append(pairs, networkProbePair{source: localInterface, local: local, target: target, remote: remote})
				if len(pairs) >= maxNetworkProbePairs {
					break
				}
			}
			if len(pairs) >= maxNetworkProbePairs {
				break
			}
		}
		if len(pairs) >= maxNetworkProbePairs {
			break
		}
	}
	results := make(chan PathProbeObservation, len(pairs))
	work := make(chan networkProbePair)
	var workers sync.WaitGroup
	count := min(networkProbeWorkers, len(pairs))
	for index := 0; index < count; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for pair := range work {
				probeContext, cancel := context.WithTimeout(ctx, networkProbeTimeout)
				latency, err := probe(probeContext, pair.local, pair.remote)
				cancel()
				result := PathProbeObservation{
					TargetNodeID:    pair.target.NodeID,
					SourceInterface: pair.source.Name, SourceAddress: pair.local.String(),
					TargetInterface: pair.target.InterfaceName, TargetAddress: pair.remote.String(),
					SourceSpeedBitsPerSecond: pair.source.SpeedBitsPerSecond,
					TargetSpeedBitsPerSecond: pair.target.SpeedBitsPerSecond,
				}
				if err == nil {
					result.Reachable = true
					result.LatencyMicros = max(1, latency.Microseconds())
				} else if errors.Is(err, context.DeadlineExceeded) {
					result.Failure = "timeout"
				} else {
					result.Failure = "unreachable"
				}
				results <- result
			}
		}()
	}
	go func() {
		defer close(work)
		for _, pair := range pairs {
			select {
			case work <- pair:
			case <-ctx.Done():
				return
			}
		}
	}()
	workers.Wait()
	close(results)
	observations := make([]PathProbeObservation, 0, len(pairs))
	for result := range results {
		observations = append(observations, result)
	}
	sort.Slice(observations, func(left, right int) bool {
		return pathProbeSortKey(observations[left]) < pathProbeSortKey(observations[right])
	})
	return observations
}

func probeAddress(raw string) (netip.Addr, bool) {
	value := strings.TrimSpace(strings.Split(raw, "/")[0])
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, false
	}
	address = address.Unmap()
	if !address.IsValid() || address.IsLoopback() || address.IsLinkLocalUnicast() ||
		address.IsMulticast() || address.IsUnspecified() {
		return netip.Addr{}, false
	}
	return address, true
}

func runPingProbe(ctx context.Context, source, target netip.Addr) (time.Duration, error) {
	executable, err := exec.LookPath("ping")
	if err != nil {
		return 0, err
	}
	command := exec.CommandContext(ctx, executable, "-n", "-c", "1", "-W", "1", "-I", source.String(), target.String())
	command.Env = append(command.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	match := pingTimePattern.FindStringSubmatch(string(output))
	if len(match) != 3 {
		return 0, fmt.Errorf("ping response omitted latency")
	}
	milliseconds, err := strconv.ParseFloat(match[2], 64)
	if err != nil {
		return 0, err
	}
	if match[1] == "<" {
		milliseconds /= 2
	}
	return time.Duration(milliseconds * float64(time.Millisecond)), nil
}

func pathProbeSortKey(probe PathProbeObservation) string {
	return strings.Join([]string{
		probe.TargetNodeID, probe.SourceInterface, probe.SourceAddress,
		probe.TargetInterface, probe.TargetAddress,
	}, "\x00")
}
