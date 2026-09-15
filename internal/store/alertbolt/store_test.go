package alertbolt_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	alertbolt "github.com/xirang-grid/xirang/internal/store/alertbolt"
)

func TestStorePersistsAndPrunesOldestResolvedAlerts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.db")
	store, err := alertbolt.Open(path, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	alerts := []domain.Alert{
		{ID: "active", Status: domain.AlertStatusFiring, LastObservedAt: base},
		{ID: "resolved-1", Status: domain.AlertStatusResolved, LastObservedAt: base.Add(time.Second), ResolvedAt: alertTime(base.Add(time.Second))},
		{ID: "resolved-2", Status: domain.AlertStatusResolved, LastObservedAt: base.Add(2 * time.Second), ResolvedAt: alertTime(base.Add(2 * time.Second))},
		{ID: "resolved-3", Status: domain.AlertStatusResolved, LastObservedAt: base.Add(3 * time.Second), ResolvedAt: alertTime(base.Add(3 * time.Second))},
	}
	for _, alert := range alerts {
		if err := store.Put(context.Background(), alert); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := alertbolt.Open(path, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("unexpected retained alerts: %#v", stored)
	}
	if _, err := reopened.Get(context.Background(), "active"); err != nil {
		t.Fatalf("active alert was pruned: %v", err)
	}
	if _, err := reopened.Get(context.Background(), "resolved-3"); err != nil {
		t.Fatalf("newest resolved alert was pruned: %v", err)
	}
}

func TestStoreAtomicallyPersistsTransitionOutboxAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.db")
	store, err := alertbolt.Open(path, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	pending := domain.Alert{ID: "alert-1", Status: domain.AlertStatusPending, Revision: 1, LastObservedAt: base}
	if err := store.Put(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	firing := pending
	firing.Status = domain.AlertStatusFiring
	firing.Revision = 2
	if err := store.PutTransition(context.Background(), firing); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTransition(context.Background(), firing); err != nil {
		t.Fatal(err)
	}
	updated := firing
	updated.Revision = 3
	updated.Severity = domain.AlertSeverityCritical
	if err := store.PutTransition(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	acknowledged := updated
	acknowledged.Revision = 4
	acknowledged.Status = domain.AlertStatusAcknowledged
	if err := store.PutTransition(context.Background(), acknowledged); err != nil {
		t.Fatal(err)
	}
	resolved := acknowledged
	resolved.Revision = 5
	resolved.Status = domain.AlertStatusResolved
	if err := store.PutTransition(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.ListDueDeliveries(context.Background(), time.Now().Add(time.Second), 10)
	if err != nil || len(deliveries) != 4 {
		t.Fatalf("unexpected transition outbox: %#v err=%v", deliveries, err)
	}
	events := make(map[string]domain.AlertDelivery)
	for _, delivery := range deliveries {
		events[delivery.EventType] = delivery
	}
	for _, eventType := range []string{"alert.firing", "alert.updated", "alert.acknowledged", "alert.resolved"} {
		if _, exists := events[eventType]; !exists {
			t.Fatalf("missing %s delivery: %#v", eventType, deliveries)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := alertbolt.Open(path, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetDelivery(context.Background(), events["alert.firing"].ID)
	if err != nil || persisted.Alert.ID != "alert-1" || persisted.State != domain.AlertDeliveryPending {
		t.Fatalf("delivery did not survive reopen: %#v err=%v", persisted, err)
	}
}

func TestStorePrunesOnlyCompletedDeliveries(t *testing.T) {
	store, err := alertbolt.Open(filepath.Join(t.TempDir(), "alerts.db"), 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UTC()
	values := []domain.AlertDelivery{
		{ID: "pending", State: domain.AlertDeliveryPending, CreatedAt: base},
		{ID: "delivered-old", State: domain.AlertDeliveryDelivered, CreatedAt: base.Add(time.Second)},
		{ID: "failed-middle", State: domain.AlertDeliveryFailed, CreatedAt: base.Add(2 * time.Second)},
		{ID: "delivered-new", State: domain.AlertDeliveryDelivered, CreatedAt: base.Add(3 * time.Second)},
	}
	for _, delivery := range values {
		if err := store.PutDelivery(context.Background(), delivery); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := store.ListDeliveries(context.Background(), 10)
	if err != nil || len(stored) != 2 {
		t.Fatalf("unexpected retained deliveries: %#v err=%v", stored, err)
	}
	if _, err := store.GetDelivery(context.Background(), "pending"); err != nil {
		t.Fatalf("pending delivery was pruned: %v", err)
	}
	if _, err := store.GetDelivery(context.Background(), "delivered-new"); err != nil {
		t.Fatalf("newest completed delivery was pruned: %v", err)
	}
}

func alertTime(value time.Time) *time.Time { return &value }
