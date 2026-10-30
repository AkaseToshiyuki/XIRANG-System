package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const healthOutboxEvents = 4096
const healthOutboxBytes = 16 << 20
const healthOutboxAge = 7 * 24 * time.Hour

var healthQueueBucket = []byte("events")
var healthQueueIndex = []byte("pending-records")
var healthQueueMeta = []byte("metadata")

type journalCheckpoint struct {
	Cursors [2]string
	Gaps    uint64
}

type healthQueueState struct {
	Journal journalCheckpoint
	SEL     []string
	Dropped uint64
	Bytes   int
	Count   int
}

type healthOutbox struct {
	db        *bbolt.DB
	maxEvents int
	maxBytes  int
	maxAge    time.Duration
}

// This database is owned by the Agent identity, and must never be reused after
// re-enrolling the state directory as another node.
func openHealthOutbox(directory, nodeID string) (*healthOutbox, error) {
	if nodeID == "" {
		return nil, errors.New("health outbox requires a node identity")
	}
	db, err := bbolt.Open(filepath.Join(directory, "health-outbox.db"), 0o600, &bbolt.Options{
		Timeout: time.Second,
		OpenFile: func(name string, flag int, mode os.FileMode) (*os.File, error) {
			file, err := os.OpenFile(name, flag|syscall.O_NOFOLLOW, mode)
			if err != nil {
				return nil, err
			}
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !healthFileOwned(info) {
				file.Close()
				return nil, errors.New("health outbox must be an owner-only regular file")
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
				file.Close()
				return nil, errors.New("health outbox must not be hard-linked")
			}
			return file, nil
		},
	})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{healthQueueBucket, healthQueueIndex, healthQueueMeta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(healthQueueMeta)
		if previous := meta.Get([]byte("node")); previous != nil && string(previous) != nodeID {
			return errors.New("health outbox belongs to another node; preserve it before re-enrollment")
		}
		return meta.Put([]byte("node"), []byte(nodeID))
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	parent, err := os.Open(directory)
	if err == nil {
		err = parent.Sync()
		parent.Close()
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("sync health outbox directory: %w", err)
	}
	return &healthOutbox{db: db, maxEvents: healthOutboxEvents, maxBytes: healthOutboxBytes, maxAge: healthOutboxAge}, nil
}

func readHealthQueueState(tx *bbolt.Tx) (healthQueueState, error) {
	var state healthQueueState
	if data := tx.Bucket(healthQueueMeta).Get([]byte("state")); data != nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return state, err
		}
	}
	return state, nil
}

func writeHealthQueueState(tx *bbolt.Tx, state healthQueueState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return tx.Bucket(healthQueueMeta).Put([]byte("state"), data)
}

func (outbox *healthOutbox) checkpoint() (healthQueueState, error) {
	var state healthQueueState
	err := outbox.db.View(func(tx *bbolt.Tx) (err error) { state, err = readHealthQueueState(tx); return err })
	return state, err
}

func eventRecordKey(event *xirangv1.SystemEvent) []byte {
	digest := sha256.Sum256([]byte(event.Source + "\x00" + event.SourceRecordId))
	return digest[:]
}

func (outbox *healthOutbox) capture(report *xirangv1.HostHealthReport, journal journalCheckpoint, now time.Time) (healthQueueState, error) {
	var committed healthQueueState
	err := outbox.db.Update(func(tx *bbolt.Tx) error {
		state, err := readHealthQueueState(tx)
		if err != nil {
			return err
		}
		previousSEL := make(map[string]bool, len(state.SEL))
		for _, id := range state.SEL {
			previousSEL[id] = true
		}
		// Keep the previous successful SEL set when access temporarily fails.
		var currentSEL []string
		queue, index := tx.Bucket(healthQueueBucket), tx.Bucket(healthQueueIndex)
		for _, event := range report.Events {
			if event.Source == "ipmi-sel" {
				currentSEL = append(currentSEL, event.SourceRecordId)
				if previousSEL[event.SourceRecordId] {
					continue
				}
			}
			id := eventRecordKey(event)
			if index.Get(id) != nil {
				continue
			}
			data, err := proto.Marshal(event)
			if err != nil {
				return err
			}
			if len(data) > 8192 {
				return errors.New("health event exceeds queue record limit")
			}
			value := make([]byte, 8, 8+len(data))
			binary.BigEndian.PutUint64(value, uint64(now.Unix()))
			value = append(value, data...)
			sequence, err := queue.NextSequence()
			if err != nil {
				return err
			}
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, sequence)
			if err := queue.Put(key, value); err != nil {
				return err
			}
			if err := index.Put(id, key); err != nil {
				return err
			}
			state.Bytes += len(value)
			state.Count++
		}
		if len(currentSEL) > 0 {
			state.SEL = currentSEL
		}
		state.Journal = journal
		if err := outbox.prune(tx, &state, now); err != nil {
			return err
		}
		if err := writeHealthQueueState(tx, state); err != nil {
			return err
		}
		committed = state
		return nil
	})
	return committed, err
}

func deleteHealthEvent(tx *bbolt.Tx, key []byte, state *healthQueueState) error {
	queue := tx.Bucket(healthQueueBucket)
	value := queue.Get(key)
	if value == nil {
		return nil
	} // May have been evicted while an ACK was in flight.
	if len(value) < 8 {
		return errors.New("invalid health queue record")
	}
	event := &xirangv1.SystemEvent{}
	if err := proto.Unmarshal(value[8:], event); err != nil {
		return err
	}
	state.Bytes -= len(value)
	state.Count--
	if err := tx.Bucket(healthQueueIndex).Delete(eventRecordKey(event)); err != nil {
		return err
	}
	return queue.Delete(key)
}

func (outbox *healthOutbox) prune(tx *bbolt.Tx, state *healthQueueState, now time.Time) error {
	queue := tx.Bucket(healthQueueBucket)
	// Host clock adjustments need not preserve capture-time order. Expire all
	// old entries before enforcing the FIFO count/byte bounds.
	var expired [][]byte
	if err := queue.ForEach(func(key, value []byte) error {
		if len(value) < 8 {
			return errors.New("invalid health queue record")
		}
		at := time.Unix(int64(binary.BigEndian.Uint64(value[:8])), 0)
		if at.Before(now.Add(-outbox.maxAge)) {
			expired = append(expired, append([]byte(nil), key...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range expired {
		if err := deleteHealthEvent(tx, key, state); err != nil {
			return err
		}
		state.Dropped++
	}
	for {
		key, _ := queue.Cursor().First()
		if key == nil {
			return nil
		}
		if state.Count <= outbox.maxEvents && state.Bytes <= outbox.maxBytes {
			return nil
		}
		if err := deleteHealthEvent(tx, append([]byte(nil), key...), state); err != nil {
			return err
		}
		state.Dropped++
	}
}

type pendingHealthBatch struct {
	message *xirangv1.HostEventBatch
	keys    [][]byte
	sentAt  time.Time
}

func (outbox *healthOutbox) batch(now time.Time) (*pendingHealthBatch, error) {
	state, err := outbox.checkpoint()
	if err != nil {
		return nil, err
	}
	if state.Count == 0 {
		return nil, nil
	}
	batch := &pendingHealthBatch{message: &xirangv1.HostEventBatch{BatchId: agentMessageID()}}
	err = outbox.db.Update(func(tx *bbolt.Tx) error {
		state, err := readHealthQueueState(tx)
		if err != nil {
			return err
		}
		if err := outbox.prune(tx, &state, now); err != nil {
			return err
		}
		if err := writeHealthQueueState(tx, state); err != nil {
			return err
		}
		cursor := tx.Bucket(healthQueueBucket).Cursor()
		for key, value := cursor.First(); key != nil && len(batch.keys) < 128; key, value = cursor.Next() {
			if len(value) < 8 {
				return errors.New("invalid health queue record")
			}
			event := &xirangv1.SystemEvent{}
			if err := proto.Unmarshal(value[8:], event); err != nil {
				return err
			}
			batch.keys = append(batch.keys, append([]byte(nil), key...))
			batch.message.Events = append(batch.message.Events, event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(batch.keys) == 0 {
		return nil, nil
	}
	return batch, nil
}

func (outbox *healthOutbox) acknowledge(batch *pendingHealthBatch, id string) error {
	if batch == nil || batch.message.BatchId != id {
		return errors.New("unexpected health event acknowledgement")
	}
	return outbox.db.Update(func(tx *bbolt.Tx) error {
		state, err := readHealthQueueState(tx)
		if err != nil {
			return err
		}
		for _, key := range batch.keys {
			if err := deleteHealthEvent(tx, key, &state); err != nil {
				return err
			}
		}
		return writeHealthQueueState(tx, state)
	})
}

// The worker lives for the control loop, not for a stream. Slow or disconnected
// controllers cannot block collection. A snapshot channel is only a wakeup;
// events remain on disk until a matching durable acknowledgement arrives.
type healthWorker struct {
	collector *hostHealthCollector
	outbox    *healthOutbox
	updates   chan struct{}
	mu        sync.RWMutex
	latest    *xirangv1.HostHealthReport
}

func (worker *healthWorker) run(ctx context.Context, interval time.Duration, onError func(string)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		state, err := worker.outbox.checkpoint()
		if err == nil {
			worker.collector.journalState = &state.Journal
			report := worker.collector.collect(ctx)
			if ctx.Err() != nil {
				return
			}
			committed, captureErr := worker.outbox.capture(report, state.Journal, time.Now())
			source := &xirangv1.HealthSource{Name: "event-outbox", Status: "available", Message: fmt.Sprintf("Durable local event queue; pending=%d; evicted unacknowledged records=%d; journal coverage warnings=%d", committed.Count, committed.Dropped, committed.Journal.Gaps)}
			if committed.Dropped > 0 || committed.Journal.Gaps > 0 {
				source.Status = "partial"
			}
			if captureErr != nil {
				err = captureErr
				source.Status, source.Message = "unavailable", "Local event queue write failed; journal cursor was not advanced"
			}
			report.Sources = append(report.Sources, source)
			worker.mu.Lock()
			worker.latest = report
			worker.mu.Unlock()
			select {
			case worker.updates <- struct{}{}:
			default:
			}
		} else {
			worker.mu.Lock()
			worker.latest = &xirangv1.HostHealthReport{ObservedAt: timestamppb.Now(), ChassisPower: "unknown", Sources: []*xirangv1.HealthSource{{Name: "event-outbox", Status: "unavailable", Message: "Local event queue read failed; collection paused without advancing cursors"}}}
			worker.mu.Unlock()
			select {
			case worker.updates <- struct{}{}:
			default:
			}
		}
		if err != nil && onError != nil {
			onError("host event queue failed; check protected state directory and available disk space")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (worker *healthWorker) snapshot(durable bool) *xirangv1.HostHealthReport {
	worker.mu.RLock()
	defer worker.mu.RUnlock()
	if worker.latest == nil {
		return nil
	}
	report := proto.Clone(worker.latest).(*xirangv1.HostHealthReport)
	if durable {
		report.Events = nil
	} else {
		report.Sources = append(report.Sources, &xirangv1.HealthSource{Name: "event-delivery", Status: "partial", Message: "Controller does not acknowledge durable event batches; local queue is retained within its limits"})
	}
	return report
}
