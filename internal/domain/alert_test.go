package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAlertJSONOmitsUnsetLifecycleTimesAndKeepsZeroMetrics(t *testing.T) {
	encoded, err := json.Marshal(Alert{
		ID: "alert-1", Status: AlertStatusFiring, Severity: AlertSeverityWarning,
		FirstObservedAt: time.Unix(1, 0).UTC(), LastObservedAt: time.Unix(2, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	for _, unwanted := range []string{"firingAt", "healthySince", "acknowledgedAt", "resolvedAt"} {
		if strings.Contains(value, unwanted) {
			t.Fatalf("unset lifecycle field %q was serialized: %s", unwanted, value)
		}
	}
	for _, required := range []string{"\"observedBits\":0", "\"droppedPacketsPerSecond\":0", "\"errorPacketsPerSecond\":0", "\"dropRatio\":0"} {
		if !strings.Contains(value, required) {
			t.Fatalf("zero metric %q was omitted: %s", required, value)
		}
	}
}
