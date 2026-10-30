package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func queueEvent(id string) *xirangv1.SystemEvent {
	return &xirangv1.SystemEvent{Source: "journal", SourceRecordId: id, Message: "corrected ECC", Kind: "corrected", Severity: "warning"}
}

func TestHealthOutboxRestartLostAcknowledgementAndSnapshotIsolation(t *testing.T) {
	directory := t.TempDir()
	outbox, err := openHealthOutbox(directory, "node")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	event := queueEvent("cursor-1")
	event.OccurredAt = timestamppb.New(now.Add(-72 * time.Hour))
	sel := queueEvent("sel-1")
	sel.Source = "ipmi-sel"
	report := &xirangv1.HostHealthReport{Events: []*xirangv1.SystemEvent{event, sel}}
	checkpoint := journalCheckpoint{Cursors: [2]string{"cursor-1", ""}}
	if _, err := outbox.capture(report, checkpoint, now); err != nil {
		t.Fatal(err)
	}
	batch, err := outbox.batch(now)
	if err != nil || batch == nil || len(batch.message.Events) != 2 {
		t.Fatalf("batch=%v err=%v", batch, err)
	}
	if err := outbox.acknowledge(batch, "wrong-session"); err == nil {
		t.Fatal("unmatched ACK accepted")
	}
	controllerPath := filepath.Join(t.TempDir(), "controller.db")
	repository, err := boltstore.Open(controllerPath)
	if err != nil {
		t.Fatal(err)
	}
	service := hosthealth.New(repository)
	current := &xirangv1.HostHealthReport{ObservedAt: timestamppb.Now(), ChassisPower: "on"}
	if err := service.Report(context.Background(), "node", current); err != nil {
		t.Fatal(err)
	}
	if err := service.CommitEvents(context.Background(), "node", batch.message); err != nil {
		t.Fatal(err)
	}
	// The controller committed, but the ACK and both processes were lost.
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := outbox.db.Close(); err != nil {
		t.Fatal(err)
	}
	outbox, err = openHealthOutbox(directory, "node")
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.db.Close()
	repository, err = boltstore.Open(controllerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service = hosthealth.New(repository)
	state, err := outbox.checkpoint()
	if err != nil || state.Journal != checkpoint {
		t.Fatalf("cursor not durable: %+v %v", state, err)
	}
	retry, err := outbox.batch(now)
	if err != nil || retry == nil || len(retry.message.Events) != 2 {
		t.Fatalf("lost pending events: %v %v", retry, err)
	}
	if err := service.CommitEvents(context.Background(), "node", retry.message); err != nil {
		t.Fatal(err)
	}
	if err := outbox.acknowledge(retry, retry.message.BatchId); err != nil {
		t.Fatal(err)
	}
	if pending, err := outbox.batch(now); err != nil || pending != nil {
		t.Fatalf("ACK did not remove events: %v %v", pending, err)
	}
	events, err := service.Events(context.Background(), "node", "", 200)
	if err != nil || len(events) != 2 {
		t.Fatalf("replay duplicated committed events: %v %v", events, err)
	}
	snapshot, err := service.Get(context.Background(), "node")
	if err != nil || snapshot.ChassisPower != "on" || !snapshot.ObservedAt.Equal(current.ObservedAt.AsTime()) {
		t.Fatal("history replaced the latest snapshot")
	}
	// Repeated unchanged hardware records do not refill an acknowledged queue.
	if _, err := outbox.capture(&xirangv1.HostHealthReport{Events: []*xirangv1.SystemEvent{sel}}, checkpoint, now); err != nil {
		t.Fatal(err)
	}
	if batch, _ := outbox.batch(now); batch != nil {
		t.Fatal("unchanged SEL re-enqueued")
	}
}

func TestHealthOutboxBoundsExpiryAtomicCursorAndIdentity(t *testing.T) {
	directory := t.TempDir()
	outbox, err := openHealthOutbox(directory, "node")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	outbox.maxEvents = 3
	report := &xirangv1.HostHealthReport{}
	for i := 0; i < 5; i++ {
		report.Events = append(report.Events, queueEvent(fmt.Sprint(i)))
	}
	checkpoint := journalCheckpoint{Cursors: [2]string{"5", ""}}
	state, err := outbox.capture(report, checkpoint, now)
	if err != nil || state.Dropped != 2 {
		t.Fatalf("count limit: %+v %v", state, err)
	}
	batch, _ := outbox.batch(now)
	if len(batch.message.Events) != 3 || batch.message.Events[0].SourceRecordId != "2" {
		t.Fatal("wrong eviction order")
	}
	oversized := queueEvent("oversized")
	oversized.Message = strings.Repeat("x", 9000)
	_, err = outbox.capture(&xirangv1.HostHealthReport{Events: []*xirangv1.SystemEvent{queueEvent("would-commit"), oversized}}, journalCheckpoint{Cursors: [2]string{"invalid", ""}}, now)
	if err == nil {
		t.Fatal("oversized event accepted")
	}
	state, _ = outbox.checkpoint()
	after, _ := outbox.batch(now)
	if state.Journal != checkpoint || len(after.message.Events) != 3 || after.message.Events[0].SourceRecordId != "2" {
		t.Fatal("failed transaction advanced cursor or mutated events")
	}
	outbox.maxBytes = 1
	if batch, err := outbox.batch(now); err != nil || batch != nil {
		t.Fatalf("byte limit: %v %v", batch, err)
	}
	state, _ = outbox.checkpoint()
	if state.Dropped != 5 || state.Bytes != 0 {
		t.Fatalf("byte accounting: %+v", state)
	}
	outbox.maxBytes = healthOutboxBytes
	if _, err := outbox.capture(&xirangv1.HostHealthReport{Events: []*xirangv1.SystemEvent{queueEvent("expires")}}, checkpoint, now.Add(-healthOutboxAge-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if batch, err := outbox.batch(now); err != nil || batch != nil {
		t.Fatal("expired event retained")
	}
	state, _ = outbox.checkpoint()
	if state.Dropped != 6 {
		t.Fatal("expiry loss not counted")
	}
	if _, err := outbox.capture(&xirangv1.HostHealthReport{Events: []*xirangv1.SystemEvent{queueEvent("new-head")}}, checkpoint, now); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.capture(&xirangv1.HostHealthReport{Events: []*xirangv1.SystemEvent{queueEvent("clock-stepped-back")}}, checkpoint, now.Add(-healthOutboxAge-time.Hour)); err != nil {
		t.Fatal(err)
	}
	remaining, err := outbox.batch(now)
	if err != nil || remaining == nil || len(remaining.message.Events) != 1 || remaining.message.Events[0].SourceRecordId != "new-head" {
		t.Fatal("clock rollback prevented expiry behind a newer head")
	}
	if err := outbox.db.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := openHealthOutbox(directory, "another-node"); err == nil {
		other.db.Close()
		t.Fatal("cross-node outbox reuse allowed")
	}
	// A real read-only database rejects the write and leaves its cursor intact.
	db, err := bbolt.Open(filepath.Join(directory, "health-outbox.db"), 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	outbox.db = db
	if _, err := outbox.capture(report, journalCheckpoint{}, now); err == nil {
		t.Fatal("read-only write succeeded")
	}
	state, _ = outbox.checkpoint()
	if state.Journal != checkpoint {
		t.Fatal("write failure advanced cursor")
	}
}

func TestHealthOutboxRefusesLinkedOrPublicFile(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "public"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			target := filepath.Join(directory, "target")
			if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "health-outbox.db")
			var err error
			switch mode {
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			case "public":
				err = os.WriteFile(path, nil, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if outbox, err := openHealthOutbox(directory, "node"); err == nil {
				outbox.db.Close()
				t.Fatal("unsafe file accepted")
			}
			data, _ := os.ReadFile(target)
			if string(data) != "sentinel" {
				t.Fatal("target modified")
			}
		})
	}
}

func TestHealthWorkerCollectsWithoutConnectionOrSnapshotConsumer(t *testing.T) {
	outbox, err := openHealthOutbox(t.TempDir(), "node")
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.db.Close()
	var sequence atomic.Int64
	collector := &hostHealthCollector{journal: "journalctl", run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "--grep=") {
			return nil, nil
		}
		var anchor string
		for _, arg := range args {
			if strings.HasPrefix(arg, "--cursor=") {
				anchor = journalTestRow(strings.TrimPrefix(arg, "--cursor="), "corrected old event")
			}
		}
		return []byte(anchor + journalTestRow(fmt.Sprint(sequence.Add(1)), "corrected new event")), nil
	}}
	worker := &healthWorker{collector: collector, outbox: outbox, updates: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.run(ctx, time.Millisecond, nil) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(3 * time.Second)
	for {
		batch, err := outbox.batch(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if batch != nil && len(batch.message.Events) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("offline collector blocked on undrained snapshot channel")
		case <-time.After(time.Millisecond):
		}
	}
	report := worker.snapshot(true)
	if report == nil || len(report.Events) != 0 {
		t.Fatal("snapshot must not replay durable events")
	}
	legacy := worker.snapshot(false)
	if legacy == nil || legacy.Sources[len(legacy.Sources)-1].Name != "event-delivery" || legacy.Sources[len(legacy.Sources)-1].Status != "partial" {
		t.Fatal("legacy delivery limitation hidden")
	}
}
