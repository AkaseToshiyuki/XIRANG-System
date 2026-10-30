package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/telemetry"
)

const metricsFreshness = 3 * time.Minute
const metricsMaximumNodes = 10000

type metricsNodeReader interface {
	ListNodes(context.Context) ([]domain.Node, error)
}

// Only cluster.read data is exported. Host logs, BMC credentials, task/user IDs,
// arbitrary inventory labels and operator repair notes never enter this endpoint.
func registerMetricsRoutes(mux *http.ServeMux, nodes metricsNodeReader, observations TelemetryReader, adminToken string) {
	mux.HandleFunc("GET /api/v1/metrics", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		defer cancel()
		items, err := nodes.ListNodes(ctx)
		if err != nil || len(items) > metricsMaximumNodes {
			http.Error(writer, "metrics snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		latest := make(map[string]domain.NodeResourceObservation, len(items))
		for _, node := range items {
			if node.Status == domain.NodeStatusRemoved {
				continue
			}
			values, err := observations.ListObservations(ctx, node.ID, 1)
			if err != nil && !errors.Is(err, telemetry.ErrNotFound) {
				http.Error(writer, "metrics snapshot unavailable", http.StatusServiceUnavailable)
				return
			}
			if len(values) > 0 {
				latest[node.ID] = values[0]
			}
		}
		if ctx.Err() != nil {
			http.Error(writer, "metrics snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		body := renderMetrics(items, latest, time.Now().UTC())
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body)
	}))
}

type metricFamily struct {
	help, kind string
	samples    []string
}

func renderMetrics(nodes []domain.Node, latest map[string]domain.NodeResourceObservation, now time.Time) []byte {
	families := map[string]*metricFamily{}
	add := func(name, help, kind, nodeID string, value float64) {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return
		}
		family := families[name]
		if family == nil {
			family = &metricFamily{help: help, kind: kind}
			families[name] = family
		}
		label := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(nodeID)
		family.samples = append(family.samples, fmt.Sprintf("%s{node_id=\"%s\"} %s\n", name, label, strconv.FormatFloat(value, 'g', -1, 64)))
	}
	for _, node := range nodes {
		if node.Status == domain.NodeStatusRemoved {
			continue
		}
		gauge := func(name, help string, value float64) { add("xirang_node_"+name, help, "gauge", node.ID, value) }
		gauge("schedulable", "Whether controller policy currently permits scheduling on this node.", boolMetric(node.Schedulable()))
		gauge("capacity_cpu_cores", "Configured CPU scheduling capacity in cores.", float64(node.Capacity.MilliCPU)/1000)
		gauge("capacity_memory_bytes", "Configured memory scheduling capacity in bytes.", float64(node.Capacity.MemoryBytes))
		gauge("allocated_cpu_cores", "CPU cores reserved by the controller.", float64(node.Allocated.MilliCPU)/1000)
		gauge("allocated_memory_bytes", "Memory bytes reserved by the controller.", float64(node.Allocated.MemoryBytes))
		if !node.LastSeenAt.IsZero() {
			gauge("heartbeat_timestamp_seconds", "Controller receipt time of the latest heartbeat as Unix seconds.", float64(node.LastSeenAt.UnixNano())/1e9)
		}
		observation, present := latest[node.ID]
		gauge("observation_present", "Whether the controller holds a resource observation.", boolMetric(present))
		fresh := present && metricTimeFresh(observation.ReceivedAt, now) && metricTimeFresh(observation.ObservedAt, now)
		gauge("observation_fresh", "Whether observation and receipt timestamps are within the last 180 seconds.", boolMetric(fresh))
		if !present {
			continue
		}
		if !observation.ObservedAt.IsZero() {
			gauge("observation_timestamp_seconds", "Agent observation time as Unix seconds; agent clock dependent.", float64(observation.ObservedAt.UnixNano())/1e9)
		}
		if !observation.ReceivedAt.IsZero() {
			gauge("observation_received_timestamp_seconds", "Controller receipt time of the resource observation as Unix seconds.", float64(observation.ReceivedAt.UnixNano())/1e9)
		}
		if !fresh {
			continue
		}
		gauge("used_cpu_cores", "Observed host CPU usage in cores.", float64(observation.UsedMilliCPU)/1000)
		gauge("used_memory_bytes", "Observed host memory use in bytes.", float64(observation.UsedMemoryBytes))
		add("xirang_node_network_received_bytes_total", "Host cumulative received bytes; may reset on reboot or interface changes.", "counter", node.ID, float64(observation.ReceivedBytes))
		add("xirang_node_network_transmitted_bytes_total", "Host cumulative transmitted bytes; may reset on reboot or interface changes.", "counter", node.ID, float64(observation.TransmittedBytes))
		gauge("cpu_temperature_available", "Whether the latest fresh CPU temperature reading is available.", boolMetric(observation.CPUTemperatureAvailable))
		if observation.CPUTemperatureAvailable {
			gauge("cpu_temperature_celsius", "Reported CPU temperature; not an independent calibration.", observation.CPUTemperatureC)
		}
		gauge("gpu_temperature_available", "Whether the latest fresh aggregate GPU temperature is available.", boolMetric(observation.GPUTemperatureAvailable))
		if observation.GPUTemperatureAvailable {
			gauge("gpu_temperature_celsius", "Reported aggregate GPU temperature.", observation.GPUTemperatureC)
		}
		gauge("gpu_utilization_available", "Whether the latest fresh aggregate GPU utilization is available.", boolMetric(observation.GPUUtilizationAvailable))
		if observation.GPUUtilizationAvailable {
			gauge("gpu_utilization_ratio", "Reported aggregate GPU utilization as a ratio.", observation.GPUUtilization/100)
		}
	}
	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)
	var result bytes.Buffer
	for _, name := range names {
		family := families[name]
		fmt.Fprintf(&result, "# HELP %s %s\n# TYPE %s %s\n", name, family.help, name, family.kind)
		sort.Strings(family.samples)
		for _, sample := range family.samples {
			result.WriteString(sample)
		}
	}
	return result.Bytes()
}

func metricTimeFresh(at, now time.Time) bool {
	return !at.IsZero() && !at.After(now) && now.Sub(at) <= metricsFreshness
}
func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
