package alertpolicy

import (
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestNotificationRoutingMatrix(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	policy := Defaults(time.Second, time.Second)
	policy.MinimumSeverity = domain.AlertSeverityWarning
	policy.Kinds = []string{"interface_health", "job_storage"}
	for _, test := range []struct {
		kind     string
		severity domain.AlertSeverity
		allowed  bool
	}{
		{"interface_health", domain.AlertSeverityInfo, false},
		{"interface_health", domain.AlertSeverityWarning, true},
		{"job_storage", domain.AlertSeverityCritical, true},
		{"group_storage", domain.AlertSeverityCritical, false},
		{"node_storage", domain.AlertSeverityCritical, false},
		{"unknown", domain.AlertSeverityCritical, false},
		{"job_storage", "unknown", false},
	} {
		if got := SuppressionReason(policy, domain.Alert{Kind: test.kind, Severity: test.severity}, now) == ""; got != test.allowed {
			t.Errorf("kind=%s severity=%s: allowed=%t", test.kind, test.severity, got)
		}
	}
	alert := domain.Alert{Kind: "job_storage", Severity: domain.AlertSeverityCritical}
	policy.SilenceUntil = &now
	if SuppressionReason(policy, alert, now) != "" {
		t.Fatal("silence must end at the specified instant")
	}
	policy.Kinds = nil
	if SuppressionReason(policy, alert, now) == "" {
		t.Fatal("empty route must not deliver")
	}
	policy = Defaults(time.Second, time.Second)
	policy.RouteEnabled = false
	if SuppressionReason(policy, alert, now) == "" {
		t.Fatal("disabled route must not deliver")
	}
}
