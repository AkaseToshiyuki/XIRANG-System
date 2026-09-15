package alerting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

func TestServiceGatesAcknowledgesAndResolvesAlert(t *testing.T) {
	repository := newMemoryRepository()
	service := NewService(repository, Config{TriggerAfter: 10 * time.Second, ResolveAfter: 10 * time.Second, RecordTransitions: true})
	base := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return base }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := service.Subscribe(ctx)

	if alerts, err := service.Evaluate(ctx, healthSnapshot("degraded")); err != nil || len(alerts) != 0 {
		t.Fatalf("pending signal became visible: alerts=%#v err=%v", alerts, err)
	}
	service.now = func() time.Time { return base.Add(9 * time.Second) }
	if alerts, err := service.Evaluate(ctx, healthSnapshot("degraded")); err != nil || len(alerts) != 0 {
		t.Fatalf("alert fired before duration: alerts=%#v err=%v", alerts, err)
	}
	service.now = func() time.Time { return base.Add(10 * time.Second) }
	alerts, err := service.Evaluate(ctx, healthSnapshot("degraded"))
	if err != nil || len(alerts) != 1 || alerts[0].Status != domain.AlertStatusFiring {
		t.Fatalf("alert did not fire: alerts=%#v err=%v", alerts, err)
	}
	if update := <-updates; update.Status != domain.AlertStatusFiring {
		t.Fatalf("unexpected firing event: %#v", update)
	}
	service.now = func() time.Time { return base.Add(10*time.Second + time.Millisecond) }
	alerts, err = service.Evaluate(ctx, healthSnapshot("critical"))
	if err != nil || len(alerts) != 1 || alerts[0].Severity != domain.AlertSeverityCritical {
		t.Fatalf("alert severity did not change in place: alerts=%#v err=%v", alerts, err)
	}
	if update := <-updates; update.Severity != domain.AlertSeverityCritical || update.ID != alerts[0].ID {
		t.Fatalf("unexpected severity event: %#v", update)
	}
	acknowledged, err := service.Acknowledge(ctx, alerts[0].ID, "test-admin")
	if err != nil || acknowledged.Status != domain.AlertStatusAcknowledged || acknowledged.AcknowledgedBy != "test-admin" {
		t.Fatalf("acknowledge failed: alert=%#v err=%v", acknowledged, err)
	}
	if update := <-updates; update.Status != domain.AlertStatusAcknowledged {
		t.Fatalf("unexpected acknowledgement event: %#v", update)
	}

	service.now = func() time.Time { return base.Add(11 * time.Second) }
	if _, err := service.Evaluate(ctx, healthSnapshot("healthy")); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(21 * time.Second) }
	if _, err := service.Evaluate(ctx, healthSnapshot("healthy")); err != nil {
		t.Fatal(err)
	}
	if update := <-updates; update.Status != domain.AlertStatusResolved {
		t.Fatalf("unexpected resolution event: %#v", update)
	}
	if active, err := service.List(ctx, false); err != nil || len(active) != 0 {
		t.Fatalf("resolved alert remained active: %#v err=%v", active, err)
	}
	history, err := service.List(ctx, true)
	if err != nil || len(history) != 1 || history[0].Status != domain.AlertStatusResolved {
		t.Fatalf("resolved history missing: %#v err=%v", history, err)
	}
	if _, err := service.Acknowledge(ctx, history[0].ID, "test-admin"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("resolved alert acknowledgement error = %v", err)
	}
	if len(repository.transitions) != 4 {
		t.Fatalf("transition outbox writes = %d, want firing, severity, acknowledgement, and resolution", len(repository.transitions))
	}
}

func TestServiceReportsAndResolvesImmediateStorageCondition(t *testing.T) {
	repository := newMemoryRepository()
	service := NewService(repository, Config{RecordTransitions: true})
	alert, err := service.ReportCondition(context.Background(), "group_storage_test", "group_storage", "", domain.AlertSeverityWarning, "quota at 80 percent")
	if err != nil || alert.Status != domain.AlertStatusFiring || alert.Severity != domain.AlertSeverityWarning {
		t.Fatalf("report condition = %#v, %v", alert, err)
	}
	alert, err = service.ReportCondition(context.Background(), "group_storage_test", "group_storage", "", domain.AlertSeverityWarning, "quota at 80 percent")
	if err != nil || alert.Revision != 2 || len(repository.transitions) != 1 {
		t.Fatalf("unchanged condition created a transition: alert=%#v transitions=%d err=%v", alert, len(repository.transitions), err)
	}
	alert, err = service.ReportCondition(context.Background(), "group_storage_test", "group_storage", "", domain.AlertSeverityCritical, "quota exceeded")
	if err != nil || alert.Revision != 3 || alert.Severity != domain.AlertSeverityCritical || len(repository.transitions) != 2 {
		t.Fatalf("severity update = %#v, %v", alert, err)
	}
	resolved, err := service.ResolveCondition(context.Background(), "group_storage_test", "usage recovered")
	if err != nil || resolved.Status != domain.AlertStatusResolved {
		t.Fatalf("resolve condition = %#v, %v", resolved, err)
	}
}

func TestServiceSuppressesTransientPendingSignal(t *testing.T) {
	repository := newMemoryRepository()
	service := NewService(repository, Config{TriggerAfter: time.Minute, ResolveAfter: time.Second})
	base := time.Now().UTC()
	service.now = func() time.Time { return base }
	if _, err := service.Evaluate(context.Background(), healthSnapshot("critical")); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(10 * time.Second) }
	if _, err := service.Evaluate(context.Background(), healthSnapshot("healthy")); err != nil {
		t.Fatal(err)
	}
	alerts, err := repository.List(context.Background())
	if err != nil || len(alerts) != 0 {
		t.Fatalf("transient pending state was retained: %#v err=%v", alerts, err)
	}
}

func TestServiceRequiresContinuousPendingObservation(t *testing.T) {
	repository := newMemoryRepository()
	service := NewService(repository, Config{TriggerAfter: 10 * time.Second, ResolveAfter: time.Second})
	base := time.Now().UTC()
	service.now = func() time.Time { return base }
	if _, err := service.Evaluate(context.Background(), healthSnapshot("degraded")); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(9 * time.Second) }
	if _, err := service.Evaluate(context.Background(), domain.TopologySnapshot{}); err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(11 * time.Second) }
	alerts, err := service.Evaluate(context.Background(), healthSnapshot("degraded"))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("interrupted signal retained its trigger clock: %#v err=%v", alerts, err)
	}
}

func TestServiceResetsOnlyPendingStateAtEvaluatorStartup(t *testing.T) {
	repository := newMemoryRepository()
	repository.alerts["pending"] = domain.Alert{ID: "pending", Status: domain.AlertStatusPending}
	repository.alerts["firing"] = domain.Alert{ID: "firing", Status: domain.AlertStatusFiring}
	service := NewService(repository, Config{})
	if err := service.ResetPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := repository.alerts["pending"]; exists {
		t.Fatal("pending state survived evaluator startup")
	}
	if _, exists := repository.alerts["firing"]; !exists {
		t.Fatal("visible firing alert was removed")
	}
}

func healthSnapshot(health string) domain.TopologySnapshot {
	return domain.TopologySnapshot{Edges: []domain.TopologyEdge{{
		ID: "edge-1", SourceID: "node-1", SourceInterface: "eth0", Health: health,
		ObservedBits: 10_000, DroppedPacketsPerSecond: 2, ErrorPacketsPerSecond: 1, DropRatio: .02,
	}}}
}

type memoryRepository struct {
	alerts      map[string]domain.Alert
	transitions []domain.Alert
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{alerts: make(map[string]domain.Alert)}
}

func (repository *memoryRepository) List(context.Context) ([]domain.Alert, error) {
	result := make([]domain.Alert, 0, len(repository.alerts))
	for _, alert := range repository.alerts {
		result = append(result, alert)
	}
	return result, nil
}

func (repository *memoryRepository) Get(_ context.Context, id string) (domain.Alert, error) {
	alert, ok := repository.alerts[id]
	if !ok {
		return domain.Alert{}, ErrNotFound
	}
	return alert, nil
}

func (repository *memoryRepository) Put(_ context.Context, alert domain.Alert) error {
	repository.alerts[alert.ID] = alert
	return nil
}

func (repository *memoryRepository) PutTransition(ctx context.Context, alert domain.Alert) error {
	repository.transitions = append(repository.transitions, alert)
	return repository.Put(ctx, alert)
}

func (repository *memoryRepository) Delete(_ context.Context, id string) error {
	delete(repository.alerts, id)
	return nil
}

func (*memoryRepository) Close() error { return nil }
