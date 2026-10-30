package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"github.com/xirang-grid/xirang/internal/telemetry"
)

func TestMetricsValuesFreshnessAndEscaping(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	id := "node\"\\\n1"
	nodes := []domain.Node{{ID: id, Status: domain.NodeStatusReady, Capacity: domain.ResourceVector{MilliCPU: 3500, MemoryBytes: 4096}}, {ID: "missing"}, {ID: "removed", Status: domain.NodeStatusRemoved}}
	latest := map[string]domain.NodeResourceObservation{id: {ObservedAt: now.Add(-time.Second), ReceivedAt: now, UsedMilliCPU: 1250, UsedMemoryBytes: 1024, ReceivedBytes: 500, CPUTemperatureAvailable: true, CPUTemperatureC: -3.5}}
	body := string(renderMetrics(nodes, latest, now))
	for _, want := range []string{
		`xirang_node_capacity_cpu_cores{node_id="node\"\\\n1"} 3.5`,
		`xirang_node_used_cpu_cores{node_id="node\"\\\n1"} 1.25`,
		`xirang_node_used_memory_bytes{node_id="node\"\\\n1"} 1024`,
		`xirang_node_cpu_temperature_celsius{node_id="node\"\\\n1"} -3.5`,
		`xirang_node_observation_present{node_id="missing"} 0`,
		`xirang_node_gpu_temperature_available{node_id="node\"\\\n1"} 0`,
		`# TYPE xirang_node_network_received_bytes_total counter`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	if strings.Contains(body, "removed") || strings.Contains(body, "# TYPE xirang_node_gpu_temperature_celsius") {
		t.Fatal("removed or unavailable data exported")
	}
	for _, delta := range []time.Duration{-metricsFreshness - time.Nanosecond, time.Nanosecond} {
		old := latest[id]
		old.ObservedAt = now.Add(delta)
		latest[id] = old
		stale := string(renderMetrics(nodes, latest, now))
		if strings.Contains(stale, "xirang_node_used_cpu_cores") || !strings.Contains(stale, `xirang_node_observation_fresh{node_id="node\"\\\n1"} 0`) {
			t.Fatal("stale/future sample looked live")
		}
	}
	old := latest[id]
	old.ObservedAt = now
	old.ReceivedAt = now.Add(-metricsFreshness)
	latest[id] = old
	if !strings.Contains(string(renderMetrics(nodes, latest, now)), "xirang_node_used_cpu_cores") {
		t.Fatal("exact freshness boundary rejected")
	}
	old.ReceivedAt = time.Time{}
	latest[id] = old
	if strings.Contains(string(renderMetrics(nodes, latest, now)), "xirang_node_used_cpu_cores") {
		t.Fatal("missing receipt time looked live")
	}
}

func TestMetricsAuthenticationAndReadOnlyToken(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	viewer, err := access.CreateToken(context.Background(), "scraper", "viewer", time.Hour, domain.Principal{Subject: "admin", Role: "admin", Permissions: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{Enrollment: enrollment.NewService(store, nil), Telemetry: &testTelemetryReader{}, Access: access, AdminToken: "admin"}).HTTPHandler()
	for _, check := range []struct {
		token string
		want  int
	}{{"", 401}, {"bad", 401}, {viewer.Secret, 200}, {"admin", 200}} {
		response := jsonRequest(t, handler, http.MethodGet, "/api/v1/metrics", nil, check.token)
		if response.Code != check.want {
			t.Fatalf("status=%d wanted=%d", response.Code, check.want)
		}
		if response.Code == 200 && (response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store") {
			t.Fatal("incorrect format/cache contract")
		}
	}
}

type metricsNodes struct {
	values []domain.Node
	err    error
}

func (reader metricsNodes) ListNodes(context.Context) ([]domain.Node, error) {
	return reader.values, reader.err
}

type metricsObservations struct{ err error }

func (reader metricsObservations) GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error) {
	return domain.NodeInventorySnapshot{}, reader.err
}
func (reader metricsObservations) ListObservations(context.Context, string, int) ([]domain.NodeResourceObservation, error) {
	return nil, reader.err
}

func TestMetricsStoreErrorsNeverReturnPartialSuccess(t *testing.T) {
	for _, check := range []struct {
		nodes          metricsNodes
		observationErr error
		want           int
	}{
		{metricsNodes{err: errors.New("database failed")}, nil, 503},
		{metricsNodes{values: make([]domain.Node, metricsMaximumNodes+1)}, nil, 503},
		{metricsNodes{values: []domain.Node{{ID: "node"}}}, errors.New("private database path"), 503},
		{metricsNodes{values: []domain.Node{{ID: "node"}}}, telemetry.ErrNotFound, 200},
	} {
		mux := http.NewServeMux()
		registerMetricsRoutes(mux, check.nodes, metricsObservations{err: check.observationErr}, "test-token")
		request := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != check.want || strings.Contains(response.Body.String(), "private database path") {
			t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
		}
		if check.want == 503 && strings.Contains(response.Body.String(), "xirang_node_") {
			t.Fatal("partial success escaped")
		}
	}
}

// Opt-in independent format validation uses the upstream executable, rather than
// a second parser derived from our renderer. No dependency is downloaded by tests.
func TestMetricsUpstreamPromtool(t *testing.T) {
	tool := os.Getenv("XIRANG_PROMTOOL")
	if tool == "" {
		t.Skip("set XIRANG_PROMTOOL for independent upstream parser validation")
	}
	now := time.Now().UTC()
	output := renderMetrics([]domain.Node{{ID: "node\"\\\n1", Status: domain.NodeStatusReady}}, map[string]domain.NodeResourceObservation{"node\"\\\n1": {ObservedAt: now, ReceivedAt: now, CPUTemperatureAvailable: true}}, now)
	command := exec.Command(tool, "check", "metrics")
	command.Stdin = bytes.NewReader(output)
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("upstream parser/linter: %v %s", err, result)
	}
}
