package external

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestBrandingHealthAndActionsSynchronizeBeforeShutdown(t *testing.T) {
	state := &fakePostgreSQL{}
	const driverName = "xirang-health-branding-test"
	sql.Register(driverName, fakeDriver{state: state})
	store, err := Open(context.Background(), Config{DSN: "postgresql://localhost/xirang?sslmode=disable", Namespace: "health", CachePath: filepath.Join(t.TempDir(), "active.db"), driver: driverName}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	verify := func(check func(*boltstore.Store)) {
		t.Helper()
		state.mu.Lock()
		data := append([]byte(nil), state.image...)
		state.mu.Unlock()
		path := filepath.Join(t.TempDir(), "snapshot.db")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		recovered, err := boltstore.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer recovered.Close()
		check(recovered)
	}
	if err := store.PutClusterBranding(ctx, 0, domain.ClusterBranding{Name: "Lab", Revision: 1}, domain.AuditEvent{ID: "branding"}); err != nil {
		t.Fatal(err)
	}
	verify(func(recovered *boltstore.Store) {
		value, err := recovered.GetClusterBranding(ctx)
		if err != nil || value.Name != "Lab" {
			t.Fatal("branding mutation did not sync")
		}
	})
	if err := store.PutAlertPolicy(ctx, 0, domain.AlertPolicy{Revision: 1, TriggerAfterSeconds: 17}, domain.AuditEvent{ID: "policy"}); err != nil {
		t.Fatal(err)
	}
	verify(func(recovered *boltstore.Store) {
		value, err := recovered.GetAlertPolicy(ctx)
		if err != nil || value.Revision != 1 || value.TriggerAfterSeconds != 17 {
			t.Fatal("alert policy mutation did not sync before shutdown")
		}
	})
	if err := store.PutNodeHealth(ctx, domain.NodeHealthSnapshot{NodeID: "node", ObservedAt: now, ReceivedAt: now}, []domain.SystemEvent{{ID: "event", NodeID: "node", Kind: "error", ReceivedAt: now}}); err != nil {
		t.Fatal(err)
	}
	verify(func(recovered *boltstore.Store) {
		if _, err := recovered.GetNodeHealth(ctx, "node"); err != nil {
			t.Fatal("health mutation did not sync")
		}
	})
	if err := store.PutSystemEvents(ctx, "node", []domain.SystemEvent{{ID: "replayed", NodeID: "node", Kind: "corrected", ReceivedAt: now}}); err != nil {
		t.Fatal(err)
	}
	verify(func(recovered *boltstore.Store) {
		items, err := recovered.ListSystemEvents(ctx, "node", "corrected", 10)
		if err != nil || len(items) != 1 || items[0].ID != "replayed" {
			t.Fatal("event batch did not synchronize before acknowledgement")
		}
	})
	if err := store.AppendSystemNote(ctx, domain.SystemEvent{ID: "repair", NodeID: "node", Kind: "repair", ReceivedAt: now}, domain.AuditEvent{ID: "note"}); err != nil {
		t.Fatal(err)
	}
	verify(func(recovered *boltstore.Store) {
		items, err := recovered.ListSystemEvents(ctx, "node", "repair", 10)
		if err != nil || len(items) != 1 {
			t.Fatal("note mutation did not sync")
		}
	})
	if err := store.PutBMCAction(ctx, domain.BMCAction{ID: "action", NodeID: "node", Status: "queued", CreatedAt: now}, domain.AuditEvent{ID: "action"}); err != nil {
		t.Fatal(err)
	}
	verify(func(recovered *boltstore.Store) {
		value, err := recovered.GetBMCAction(ctx, "action")
		if err != nil || value.Status != "queued" {
			t.Fatal("action mutation did not sync")
		}
	})
}
