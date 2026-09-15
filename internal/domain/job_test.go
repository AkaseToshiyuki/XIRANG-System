package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestLegacyJobJSONKeepsSingleNodeParallelDefaults(t *testing.T) {
	const legacy = `{
		"id":"job-legacy","projectId":"legacy","status":"queued",
		"task":{"artifact":{"uri":"file:///tmp/task","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","architecture":"amd64","entrypoint":"task"},"resources":{"milliCpu":1000,"memoryBytes":1073741824}}
	}`
	var job domain.Job
	if err := json.Unmarshal([]byte(legacy), &job); err != nil {
		t.Fatal(err)
	}
	parallel := job.Task.Parallel.Effective()
	if parallel.Nodes != 1 || parallel.TasksPerNode != 1 || parallel.Launcher != "native" || job.Allocation != nil {
		t.Fatalf("legacy job changed meaning after R3 decode: job=%#v parallel=%#v", job, parallel)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	task := roundTrip["task"].(map[string]any)
	parallelJSON, present := task["parallel"].(map[string]any)
	if !present || len(parallelJSON) != 0 {
		t.Fatalf("legacy JSON should expose only an empty additive parallel object: %s", encoded)
	}
}
