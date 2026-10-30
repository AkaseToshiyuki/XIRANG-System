package bolt

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"go.etcd.io/bbolt"
)

func (store *Store) PutNodeHealth(ctx context.Context, snapshot domain.NodeHealthSnapshot, events []domain.SystemEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(hostHealthBucket)
		if encoded := bucket.Get([]byte(snapshot.NodeID)); encoded != nil {
			var previous domain.NodeHealthSnapshot
			if err := json.Unmarshal(encoded, &previous); err != nil {
				return err
			}
			if previous.ObservedAt.After(snapshot.ObservedAt) {
				return nil
			}
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(snapshot.NodeID), data); err != nil {
			return err
		}
		return putSystemEvents(tx, snapshot.NodeID, events)
	})
}

func (store *Store) PutSystemEvents(ctx context.Context, nodeID string, events []domain.SystemEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error { return putSystemEvents(tx, nodeID, events) })
}

func putSystemEvents(tx *bbolt.Tx, nodeID string, events []domain.SystemEvent) error {
	bucket, err := tx.Bucket(systemEventsBucket).CreateBucketIfNotExists([]byte(nodeID))
	if err != nil {
		return err
	}
	for _, event := range events {
		if bucket.Get([]byte(event.ID)) != nil {
			continue
		}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(event.ID), data); err != nil {
			return err
		}
	}
	var retained []domain.SystemEvent
	cutoff := time.Now().Add(-hosthealth.Retention)
	if err := bucket.ForEach(func(key, data []byte) error {
		var event domain.SystemEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if event.ReceivedAt.Before(cutoff) {
			return nil
		}
		retained = append(retained, event)
		return nil
	}); err != nil {
		return err
	}
	sortEvents(retained)
	if len(retained) > hosthealth.RetainedEvents {
		retained = retained[:hosthealth.RetainedEvents]
	}
	keep := map[string]bool{}
	for _, event := range retained {
		keep[event.ID] = true
	}
	var remove [][]byte
	if err := bucket.ForEach(func(key, _ []byte) error {
		if !keep[string(key)] {
			remove = append(remove, append([]byte(nil), key...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range remove {
		if err := bucket.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) GetNodeHealth(ctx context.Context, nodeID string) (domain.NodeHealthSnapshot, error) {
	var value domain.NodeHealthSnapshot
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := store.database.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(hostHealthBucket).Get([]byte(nodeID))
		if data == nil {
			return hosthealth.ErrNotFound
		}
		return json.Unmarshal(data, &value)
	})
	return value, err
}

func (store *Store) ListSystemEvents(ctx context.Context, nodeID, kind string, limit int) ([]domain.SystemEvent, error) {
	items := []domain.SystemEvent{}
	if err := ctx.Err(); err != nil {
		return items, err
	}
	err := store.database.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(systemEventsBucket).Bucket([]byte(nodeID))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, data []byte) error {
			var event domain.SystemEvent
			if err := json.Unmarshal(data, &event); err != nil {
				return err
			}
			if (kind == "" || event.Kind == kind) && !event.ReceivedAt.Before(time.Now().Add(-hosthealth.Retention)) {
				items = append(items, event)
			}
			return nil
		})
	})
	sortEvents(items)
	if len(items) > limit {
		items = items[:limit]
	}
	return items, err
}

func sortEvents(items []domain.SystemEvent) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i].ReceivedAt, items[j].ReceivedAt
		if items[i].OccurredAt != nil {
			a = *items[i].OccurredAt
		}
		if items[j].OccurredAt != nil {
			b = *items[j].OccurredAt
		}
		if a.Equal(b) {
			return items[i].ID < items[j].ID
		}
		return a.After(b)
	})
}

func (store *Store) AppendSystemNote(ctx context.Context, event domain.SystemEvent, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		if event.RelatedEventID != "" {
			bucket := tx.Bucket(systemEventsBucket).Bucket([]byte(event.NodeID))
			if bucket == nil || bucket.Get([]byte(event.RelatedEventID)) == nil {
				return hosthealth.ErrNotFound
			}
		}
		if err := putSystemEvents(tx, event.NodeID, []domain.SystemEvent{event}); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}

func (store *Store) PutBMCAction(ctx context.Context, value domain.BMCAction, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bmcActionsBucket)
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(value.ID), data); err != nil {
			return err
		}
		if err := putAudit(tx, audit); err != nil {
			return err
		}
		// Bound the operational status cache. The separate audit trail remains.
		if bucket.Stats().KeyN > 1000 {
			var actions []domain.BMCAction
			if err := bucket.ForEach(func(_, data []byte) error {
				var action domain.BMCAction
				if err := json.Unmarshal(data, &action); err != nil {
					return err
				}
				actions = append(actions, action)
				return nil
			}); err != nil {
				return err
			}
			sort.Slice(actions, func(i, j int) bool { return actions[i].CreatedAt.Before(actions[j].CreatedAt) })
			for _, action := range actions[:len(actions)-1000] {
				if err := bucket.Delete([]byte(action.ID)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (store *Store) GetBMCAction(ctx context.Context, id string) (domain.BMCAction, error) {
	var value domain.BMCAction
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := store.database.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bmcActionsBucket).Get([]byte(id))
		if data == nil {
			return hosthealth.ErrNotFound
		}
		return json.Unmarshal(data, &value)
	})
	return value, err
}
