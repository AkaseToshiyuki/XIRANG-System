package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type prometheusFixtureReader struct{ phase atomic.Int32 }

func (*prometheusFixtureReader) GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error) {
	return domain.NodeInventorySnapshot{}, nil
}
func (reader *prometheusFixtureReader) ListObservations(context.Context, string, int) ([]domain.NodeResourceObservation, error) {
	phase := reader.phase.Load()
	if phase == 1 {
		return nil, errors.New("isolated database outage")
	}
	now := time.Now().UTC()
	observed := now.Add(-time.Second)
	if phase == 3 {
		observed = now.Add(-4 * time.Minute)
	}
	cpu := int64(1250)
	if phase >= 2 {
		cpu = 2000
	}
	return []domain.NodeResourceObservation{{NodeID: "node-live", ObservedAt: observed, ReceivedAt: now, UsedMilliCPU: cpu}}, nil
}

// Opt-in, entirely loopback: use upstream Prometheus to verify HTTPS/auth scrape,
// source failure, stale-data omission, recovery and TSDB/WAL restart persistence.
func TestMetricsPrometheusPersistence(t *testing.T) {
	program := os.Getenv("XIRANG_PROMETHEUS")
	if program == "" {
		t.Skip("set XIRANG_PROMETHEUS to run isolated upstream storage acceptance")
	}
	root := t.TempDir()
	reader := &prometheusFixtureReader{}
	mux := http.NewServeMux()
	registerMetricsRoutes(mux, metricsNodes{values: []domain.Node{{ID: "node-live", Status: domain.NodeStatusReady}}}, reader, "isolated-metrics-test")
	source := httptest.NewTLSServer(mux)
	defer source.Close()
	ca := filepath.Join(root, "source-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: source.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("global:\n  scrape_interval: 1s\n  scrape_timeout: 500ms\nscrape_configs:\n  - job_name: xirang\n    scheme: https\n    metrics_path: /api/v1/metrics\n    authorization:\n      credentials: isolated-metrics-test\n    tls_config:\n      ca_file: %s\n    static_configs:\n      - targets: ['%s']\n", ca, strings.TrimPrefix(source.URL, "https://"))
	configPath := filepath.Join(root, "prometheus.yml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	client := &http.Client{Timeout: time.Second}
	log, err := os.OpenFile(filepath.Join(root, "prometheus.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var command *exec.Cmd
	var done chan error
	start := func() {
		command = exec.Command(program, "--config.file="+configPath, "--storage.tsdb.path="+filepath.Join(root, "db"), "--storage.tsdb.retention.time=7d", "--storage.tsdb.retention.size=64MB", "--web.listen-address="+address, "--log.level=error")
		command.Stdout, command.Stderr = log, log
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done = make(chan error, 1)
		current := command
		result := done
		go func() { result <- current.Wait() }()
	}
	stop := func() {
		if command == nil {
			return
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
		command = nil
	}
	defer stop()
	query := func(expression string) (float64, error) {
		response, err := client.Get("http://" + address + "/api/v1/query?query=" + url.QueryEscape(expression))
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		var payload struct {
			Status string
			Data   struct {
				Result []struct{ Value []json.RawMessage }
			}
		}
		if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
			return 0, err
		}
		if payload.Status != "success" || len(payload.Data.Result) != 1 || len(payload.Data.Result[0].Value) != 2 {
			return 0, errors.New("no single query sample")
		}
		var text string
		if err = json.Unmarshal(payload.Data.Result[0].Value[1], &text); err != nil {
			return 0, err
		}
		return strconv.ParseFloat(text, 64)
	}
	waitValue := func(label, expression string, expected float64) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			value, err := query(expression)
			if err == nil && value == expected {
				t.Log(label)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		data, _ := os.ReadFile(filepath.Join(root, "prometheus.log"))
		t.Fatalf("%s did not reach %g: %s", label, expected, data)
	}
	start()
	metric := `xirang_node_used_cpu_cores{node_id="node-live"}`
	waitValue("HTTPS and bearer scrape returned 1.25 CPU cores", metric, 1.25)
	waitValue("multiple original samples stored", `count_over_time(`+metric+`[5m]) >= bool 2`, 1)
	reader.phase.Store(1)
	waitValue("source store failure becomes failed scrape", `up{job="xirang"}`, 0)
	waitValue("failed scrape does not keep old usage current", `absent(`+metric+`)`, 1)
	reader.phase.Store(2)
	waitValue("recovered scrape contains new usage", metric, 2)
	reader.phase.Store(3)
	waitValue("stale observation is explicitly marked", `xirang_node_observation_fresh{node_id="node-live"}`, 0)
	waitValue("stale usage is omitted", `absent(`+metric+`)`, 1)
	waitValue("stale source data is distinct from failed scrape", `up{job="xirang"}`, 1)
	reader.phase.Store(2)
	waitValue("fresh source resumes export", metric, 2)
	stop()
	start()
	waitValue("historical samples survive Prometheus restart", `min_over_time(`+metric+`[5m])`, 1.25)
	waitValue("scraping resumes after restart", metric, 2)
}
