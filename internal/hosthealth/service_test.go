package hosthealth_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHostHealthPersistsDeduplicatesAndKeepsRepairSeparate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := hosthealth.New(store)
	report := &xirangv1.HostHealthReport{ObservedAt: timestamppb.Now(), Sources: []*xirangv1.HealthSource{{Name: "journal", Status: "available"}}, Events: []*xirangv1.SystemEvent{{Source: "journal", SourceRecordId: "cursor-1", Kind: "corrected", Severity: "warning", Message: "corrected ECC"}}}
	for i := 0; i < 2; i++ {
		if err := service.Report(ctx, "node-a", report); err != nil {
			t.Fatal(err)
		}
	}
	events, err := service.Events(ctx, "node-a", "", 200)
	if err != nil || len(events) != 1 {
		t.Fatalf("duplicate report: %v %v", events, err)
	}
	if err := service.Report(ctx, "node-b", report); err != nil {
		t.Fatal(err)
	}
	other, _ := service.Events(ctx, "node-b", "", 200)
	if other[0].ID == events[0].ID || other[0].NodeID != "node-b" {
		t.Fatal("node identity was not bound to source event")
	}
	if _, err := service.AddNote(ctx, "node-a", "inspected DIMM", events[0].ID, domain.Principal{Role: "viewer"}); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("non-admin repair: %v", err)
	}
	if _, err := service.AddNote(ctx, "node-a", "replaced DIMM; memory diagnostics passed", events[0].ID, domain.Principal{Role: "admin", Subject: "maintainer"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service = hosthealth.New(store)
	events, err = service.Events(ctx, "node-a", "", 200)
	if err != nil || len(events) != 2 {
		t.Fatalf("restart: %v %v", events, err)
	}
	corrected, _ := service.Events(ctx, "node-a", "corrected", 200)
	if len(corrected) != 1 || corrected[0].Actor != "" {
		t.Fatal("repair replaced the original error event")
	}
	if _, err := service.Events(ctx, "node-a", "", 201); !errors.Is(err, hosthealth.ErrInvalid) {
		t.Fatal("unbounded event query accepted")
	}
	for _, invalid := range []*xirangv1.HostHealthReport{
		{ObservedAt: timestamppb.Now(), Sensors: []*xirangv1.HardwareSensor{{Id: "sensor", Name: "bad", Source: "ipmi", Value: math.NaN()}}},
		{ObservedAt: timestamppb.Now(), Events: []*xirangv1.SystemEvent{{Source: "journal", SourceRecordId: "bad", Kind: "repair", Severity: "info", Message: "forged operator repair"}}},
		{ObservedAt: timestamppb.New(time.Now().Add(time.Hour))},
		{ObservedAt: timestamppb.Now(), Events: []*xirangv1.SystemEvent{{Source: "operator", SourceRecordId: "fake", Kind: "info", Severity: "info", Message: "spoofed source"}}},
	} {
		if err := service.Report(ctx, "node-a", invalid); !errors.Is(err, hosthealth.ErrInvalid) {
			t.Fatalf("invalid agent input accepted: %v", err)
		}
	}
}

func TestHostHealthRetentionBoundAndOldReport(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	snapshot := domain.NodeHealthSnapshot{NodeID: "node", ObservedAt: now, ReceivedAt: now, ChassisPower: "on"}
	events := make([]domain.SystemEvent, 2005)
	for i := range events {
		at := now.Add(-time.Duration(i) * time.Second)
		events[i] = domain.SystemEvent{ID: fmt.Sprint(i), NodeID: "node", Kind: "error", Message: "error", ReceivedAt: at}
	}
	events = append(events, domain.SystemEvent{ID: "expired", NodeID: "node", ReceivedAt: now.Add(-31 * 24 * time.Hour)})
	if err := store.PutNodeHealth(context.Background(), snapshot, events); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListSystemEvents(context.Background(), "node", "", 3000)
	if err != nil || len(items) != 2000 || items[0].ID != "0" || items[1999].ID != "1999" {
		t.Fatalf("retention: %d %v", len(items), err)
	}
	snapshot.ObservedAt = now.Add(-time.Minute)
	snapshot.ChassisPower = "off"
	if err := store.PutNodeHealth(context.Background(), snapshot, nil); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetNodeHealth(context.Background(), "node")
	if err != nil || current.ChassisPower != "on" {
		t.Fatalf("old report replaced newer observation: %v %v", current, err)
	}
}
