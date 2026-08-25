package alertbolt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/xirang-grid/xirang/internal/alerting"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/notification"
	"go.etcd.io/bbolt"
)

var (
	alertsBucket     = []byte("alerts")
	deliveriesBucket = []byte("alert-deliveries")
)

const defaultRetention = 1000

type Store struct {
	database          *bbolt.DB
	retention         int
	deliveryRetention int
}

func Open(path string, retention, deliveryRetention int) (*Store, error) {
	if retention <= 0 {
		retention = defaultRetention
	}
	if deliveryRetention <= 0 {
		deliveryRetention = defaultRetention * 2
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	database, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := database.Update(func(transaction *bbolt.Tx) error {
		if _, err := transaction.CreateBucketIfNotExists(alertsBucket); err != nil {
			return err
		}
		_, err := transaction.CreateBucketIfNotExists(deliveriesBucket)
		return err
	}); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &Store{database: database, retention: retention, deliveryRetention: deliveryRetention}, nil
}

func (store *Store) Close() error { return store.database.Close() }

func (store *Store) Put(ctx context.Context, alert domain.Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return putAlert(transaction.Bucket(alertsBucket), alert, store.retention)
	})
}

func (store *Store) PutTransition(ctx context.Context, alert domain.Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		alerts := transaction.Bucket(alertsBucket)
		previous := domain.Alert{}
		if encoded := alerts.Get([]byte(alert.ID)); encoded != nil {
			if err := json.Unmarshal(encoded, &previous); err != nil {
				return err
			}
		}
		if err := putAlert(alerts, alert, store.retention); err != nil {
			return err
		}
		delivery := newDelivery(previous, alert, time.Now().UTC())
		deliveries := transaction.Bucket(deliveriesBucket)
		if deliveries.Get([]byte(delivery.ID)) == nil {
			encoded, err := json.Marshal(delivery)
			if err != nil {
				return err
			}
			if err := deliveries.Put([]byte(delivery.ID), encoded); err != nil {
				return err
			}
		}
		return pruneDeliveries(deliveries, store.deliveryRetention)
	})
}

func (store *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(alertsBucket).Delete([]byte(id))
	})
}

func (store *Store) Get(ctx context.Context, id string) (domain.Alert, error) {
	if err := ctx.Err(); err != nil {
		return domain.Alert{}, err
	}
	var alert domain.Alert
	err := store.database.View(func(transaction *bbolt.Tx) error {
		encoded := transaction.Bucket(alertsBucket).Get([]byte(id))
		if encoded == nil {
			return alerting.ErrNotFound
		}
		return json.Unmarshal(encoded, &alert)
	})
	return alert, err
}

func (store *Store) List(ctx context.Context) ([]domain.Alert, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var alerts []domain.Alert
	err := store.database.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(alertsBucket).ForEach(func(_, encoded []byte) error {
			var alert domain.Alert
			if err := json.Unmarshal(encoded, &alert); err != nil {
				return err
			}
			alerts = append(alerts, alert)
			return nil
		})
	})
	sort.Slice(alerts, func(left, right int) bool { return alerts[left].LastObservedAt.After(alerts[right].LastObservedAt) })
	return alerts, err
}

func (store *Store) ListDeliveries(ctx context.Context, limit int) ([]domain.AlertDelivery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	deliveries, err := store.readDeliveries(func(domain.AlertDelivery) bool { return true })
	if len(deliveries) > limit {
		deliveries = deliveries[:limit]
	}
	return deliveries, err
}

func (store *Store) ListDueDeliveries(ctx context.Context, now time.Time, limit int) ([]domain.AlertDelivery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 32
	}
	deliveries, err := store.readDeliveries(func(delivery domain.AlertDelivery) bool {
		if delivery.State != domain.AlertDeliveryPending && delivery.State != domain.AlertDeliveryRetrying {
			return false
		}
		return delivery.NextAttemptAt == nil || !delivery.NextAttemptAt.After(now)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(deliveries, func(left, right int) bool {
		return deliverySchedule(deliveries[left]).Before(deliverySchedule(deliveries[right]))
	})
	if len(deliveries) > limit {
		deliveries = deliveries[:limit]
	}
	return deliveries, nil
}

func (store *Store) GetDelivery(ctx context.Context, id string) (domain.AlertDelivery, error) {
	if err := ctx.Err(); err != nil {
		return domain.AlertDelivery{}, err
	}
	var delivery domain.AlertDelivery
	err := store.database.View(func(transaction *bbolt.Tx) error {
		encoded := transaction.Bucket(deliveriesBucket).Get([]byte(id))
		if encoded == nil {
			return notification.ErrNotFound
		}
		return json.Unmarshal(encoded, &delivery)
	})
	return delivery, err
}

func (store *Store) PutDelivery(ctx context.Context, delivery domain.AlertDelivery) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(deliveriesBucket)
		if err := bucket.Put([]byte(delivery.ID), encoded); err != nil {
			return err
		}
		return pruneDeliveries(bucket, store.deliveryRetention)
	})
}

func (store *Store) readDeliveries(include func(domain.AlertDelivery) bool) ([]domain.AlertDelivery, error) {
	var deliveries []domain.AlertDelivery
	err := store.database.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(deliveriesBucket).ForEach(func(_, encoded []byte) error {
			var delivery domain.AlertDelivery
			if err := json.Unmarshal(encoded, &delivery); err != nil {
				return err
			}
			if include(delivery) {
				deliveries = append(deliveries, delivery)
			}
			return nil
		})
	})
	sort.Slice(deliveries, func(left, right int) bool {
		if deliveries[left].CreatedAt.Equal(deliveries[right].CreatedAt) {
			return deliveries[left].AlertRevision > deliveries[right].AlertRevision
		}
		return deliveries[left].CreatedAt.After(deliveries[right].CreatedAt)
	})
	return deliveries, err
}

func putAlert(bucket *bbolt.Bucket, alert domain.Alert, retention int) error {
	encoded, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	if err := bucket.Put([]byte(alert.ID), encoded); err != nil {
		return err
	}
	return pruneResolved(bucket, retention)
}

func newDelivery(previous, alert domain.Alert, now time.Time) domain.AlertDelivery {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", alert.ID, alert.Revision)))
	eventType := "alert.updated"
	switch {
	case alert.Status == domain.AlertStatusFiring && previous.Status != domain.AlertStatusFiring:
		eventType = "alert.firing"
	case alert.Status == domain.AlertStatusAcknowledged && previous.Status != domain.AlertStatusAcknowledged:
		eventType = "alert.acknowledged"
	case alert.Status == domain.AlertStatusResolved && previous.Status != domain.AlertStatusResolved:
		eventType = "alert.resolved"
	}
	return domain.AlertDelivery{
		ID: fmt.Sprintf("delivery_%x", digest[:12]), EventType: eventType,
		AlertID: alert.ID, AlertRevision: alert.Revision, Alert: alert,
		State: domain.AlertDeliveryPending, CreatedAt: now, NextAttemptAt: &now,
	}
}

func deliverySchedule(delivery domain.AlertDelivery) time.Time {
	if delivery.NextAttemptAt != nil {
		return *delivery.NextAttemptAt
	}
	return delivery.CreatedAt
}

func pruneResolved(bucket *bbolt.Bucket, retention int) error {
	type resolvedAlert struct {
		id string
		at time.Time
	}
	count := 0
	resolved := make([]resolvedAlert, 0)
	if err := bucket.ForEach(func(key, encoded []byte) error {
		count++
		var alert domain.Alert
		if err := json.Unmarshal(encoded, &alert); err != nil {
			return err
		}
		if alert.Status == domain.AlertStatusResolved {
			resolvedAt := alert.LastObservedAt
			if alert.ResolvedAt != nil {
				resolvedAt = *alert.ResolvedAt
			}
			resolved = append(resolved, resolvedAlert{id: string(key), at: resolvedAt})
		}
		return nil
	}); err != nil {
		return err
	}
	overflow := count - retention
	if overflow <= 0 {
		return nil
	}
	sort.Slice(resolved, func(left, right int) bool { return resolved[left].at.Before(resolved[right].at) })
	for index := 0; index < overflow && index < len(resolved); index++ {
		if err := bucket.Delete([]byte(resolved[index].id)); err != nil {
			return err
		}
	}
	return nil
}

func pruneDeliveries(bucket *bbolt.Bucket, retention int) error {
	type completedDelivery struct {
		id string
		at time.Time
	}
	count := 0
	completed := make([]completedDelivery, 0)
	if err := bucket.ForEach(func(key, encoded []byte) error {
		count++
		var delivery domain.AlertDelivery
		if err := json.Unmarshal(encoded, &delivery); err != nil {
			return err
		}
		if delivery.State == domain.AlertDeliveryDelivered || delivery.State == domain.AlertDeliveryFailed {
			completed = append(completed, completedDelivery{id: string(key), at: delivery.CreatedAt})
		}
		return nil
	}); err != nil {
		return err
	}
	overflow := count - retention
	if overflow <= 0 {
		return nil
	}
	sort.Slice(completed, func(left, right int) bool { return completed[left].at.Before(completed[right].at) })
	for index := 0; index < overflow && index < len(completed); index++ {
		if err := bucket.Delete([]byte(completed[index].id)); err != nil {
			return err
		}
	}
	return nil
}
