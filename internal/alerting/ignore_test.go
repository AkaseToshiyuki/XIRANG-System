package alerting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestIgnoredInterfaceIncidentRecoversAndRecurrenceFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := newMemoryRepository()
	service := NewService(repo, Config{TriggerAfter: time.Second, ResolveAfter: time.Second, RecordTransitions: true})
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	if _, err := service.Evaluate(ctx, healthSnapshot("critical")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	active, err := service.Evaluate(ctx, healthSnapshot("critical"))
	if err != nil || len(active) != 1 {
		t.Fatalf("firing: %+v %v", active, err)
	}
	alert := active[0]
	if _, err := service.Ignore(ctx, alert.ID, "alice", alert.Revision+1); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale ignore accepted: %v", err)
	}
	// Ordinary observations advance row revision without changing the reviewed condition.
	now = now.Add(time.Second)
	if _, err := service.Evaluate(ctx, healthSnapshot("critical")); err != nil {
		t.Fatal(err)
	}
	updates := service.Subscribe(ctx)
	ignored, err := service.Ignore(ctx, alert.ID, "alice", alert.Revision)
	if err != nil || ignored.Status != domain.AlertStatusIgnored || ignored.IgnoredBy != "alice" || ignored.IgnoredAt == nil || ignored.ResolvedAt != nil || ignored.ObservedHealth != "critical" {
		t.Fatalf("ignore changed evidence: %+v %v", ignored, err)
	}
	select {
	case update := <-updates:
		if update.Status != domain.AlertStatusIgnored || update.IgnoredBy != "alice" {
			t.Fatalf("ignore event: %+v", update)
		}
	default:
		t.Fatal("ignore was not published")
	}
	if again, err := service.Ignore(ctx, alert.ID, "bob", ignored.Revision); err != nil || again.IgnoredBy != "alice" || again.Revision != ignored.Revision {
		t.Fatalf("duplicate ignore changed record: %+v %v", again, err)
	}
	if active, err := service.Evaluate(ctx, healthSnapshot("critical")); err != nil || len(active) != 0 {
		t.Fatalf("same ignored incident fired: %+v %v", active, err)
	}
	history, err := service.List(ctx, true)
	if err != nil || len(history) != 1 || history[0].Status != domain.AlertStatusIgnored {
		t.Fatalf("ignored history lost: %+v %v", history, err)
	}
	now = now.Add(time.Second)
	if _, err := service.Evaluate(ctx, healthSnapshot("healthy")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := service.Evaluate(ctx, healthSnapshot("healthy")); err != nil {
		t.Fatal(err)
	}
	history, _ = service.List(ctx, true)
	if history[0].Status != domain.AlertStatusResolved || history[0].ResolvedAt == nil {
		t.Fatalf("ignored condition did not resolve: %+v", history)
	}
	if _, err := service.Ignore(ctx, alert.ID, "alice", history[0].Revision); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("resolved alert ignored: %v", err)
	}
	now = now.Add(time.Second)
	if _, err := service.Evaluate(ctx, healthSnapshot("critical")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	active, err = service.Evaluate(ctx, healthSnapshot("critical"))
	if err != nil || len(active) != 1 || active[0].ID == alert.ID || active[0].Status != domain.AlertStatusFiring {
		t.Fatalf("new incident stayed ignored: %+v %v", active, err)
	}
}

func TestIgnoredConditionsReopenOnSeverityEscalation(t *testing.T) {
	ctx := context.Background()
	for _, operational := range []bool{false, true} {
		repo := newMemoryRepository()
		service := NewService(repo, Config{TriggerAfter: time.Second, RecordTransitions: true})
		now := time.Now().UTC()
		service.now = func() time.Time { return now }
		observe := func(critical bool) domain.Alert {
			t.Helper()
			if operational {
				severity := domain.AlertSeverityWarning
				if critical {
					severity = domain.AlertSeverityCritical
				}
				alert, err := service.ReportCondition(ctx, "storage-node", "node_storage", "node", severity, "capacity changed")
				if err != nil {
					t.Fatal(err)
				}
				return alert
			}
			health := "degraded"
			if critical {
				health = "critical"
			}
			if _, err := service.Evaluate(ctx, healthSnapshot(health)); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Second)
			alerts, err := service.Evaluate(ctx, healthSnapshot(health))
			if err != nil || len(alerts) != 1 {
				t.Fatalf("evaluate: %+v %v", alerts, err)
			}
			return alerts[0]
		}
		alert := observe(false)
		ack, err := service.Acknowledge(ctx, alert.ID, "first-operator")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Ignore(ctx, alert.ID, "second-operator", ack.Revision); err != nil {
			t.Fatal(err)
		}
		if operational {
			continued, err := service.ReportCondition(ctx, "storage-node", "node_storage", "node", domain.AlertSeverityWarning, "capacity changed again")
			if err != nil || continued.Status != domain.AlertStatusIgnored {
				t.Fatalf("summary update undid ignore: %+v %v", continued, err)
			}
		}
		reopened := observe(true)
		if _, err := service.Ignore(ctx, reopened.ID, "late-operator", ack.Revision); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("stale confirmation hid escalated condition: %v", err)
		}
		if reopened.ID != alert.ID || reopened.Status != domain.AlertStatusFiring || reopened.IgnoredAt != nil || reopened.AcknowledgedAt != nil {
			t.Fatalf("escalation remained ignored: %+v", reopened)
		}
	}
}
