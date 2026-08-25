package telemetrybolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/telemetry"
	"go.etcd.io/bbolt"
)

var (
	inventoryBucket    = []byte("inventory")
	observationsBucket = []byte("observations")
)

const defaultRetention = 720

type Store struct {
	database  *bbolt.DB
	retention int
}

func Open(path string, retention int) (*Store, error) {
	if retention <= 0 {
		retention = defaultRetention
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	database, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	store := &Store{database: database, retention: retention}
	if err := database.Update(func(transaction *bbolt.Tx) error {
		if _, err := transaction.CreateBucketIfNotExists(inventoryBucket); err != nil {
			return err
		}
		_, err := transaction.CreateBucketIfNotExists(observationsBucket)
		return err
	}); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error { return store.database.Close() }

func (store *Store) PutInventory(ctx context.Context, inventory domain.NodeInventorySnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(inventoryBucket).Put([]byte(inventory.NodeID), encoded)
	})
}

func (store *Store) GetInventory(ctx context.Context, nodeID string) (domain.NodeInventorySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.NodeInventorySnapshot{}, err
	}
	var inventory domain.NodeInventorySnapshot
	err := store.database.View(func(transaction *bbolt.Tx) error {
		encoded := transaction.Bucket(inventoryBucket).Get([]byte(nodeID))
		if encoded == nil {
			return telemetry.ErrNotFound
		}
		return json.Unmarshal(encoded, &inventory)
	})
	return inventory, err
}

func (store *Store) AppendObservation(ctx context.Context, observation domain.NodeResourceObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		parent := transaction.Bucket(observationsBucket)
		bucket, err := parent.CreateBucketIfNotExists([]byte(observation.NodeID))
		if err != nil {
			return err
		}
		key := observationKey(observation.ObservedAt, observation.MessageID)
		keyCount := bucket.Stats().KeyN
		if bucket.Get(key) == nil {
			keyCount++
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
		for keyCount > store.retention {
			oldest, _ := bucket.Cursor().First()
			if oldest == nil {
				break
			}
			if err := bucket.Delete(oldest); err != nil {
				return err
			}
			keyCount--
		}
		return nil
	})
}

func (store *Store) ListObservations(ctx context.Context, nodeID string, limit int) ([]domain.NodeResourceObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	result := make([]domain.NodeResourceObservation, 0, limit)
	err := store.database.View(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(observationsBucket).Bucket([]byte(nodeID))
		if bucket == nil {
			return telemetry.ErrNotFound
		}
		cursor := bucket.Cursor()
		for _, encoded := cursor.Last(); encoded != nil && len(result) < limit; _, encoded = cursor.Prev() {
			var observation domain.NodeResourceObservation
			if err := json.Unmarshal(encoded, &observation); err != nil {
				return err
			}
			result = append(result, observation)
		}
		return nil
	})
	return result, err
}

func observationKey(observedAt time.Time, messageID string) []byte {
	key := make([]byte, 8+len(messageID))
	binary.BigEndian.PutUint64(key, uint64(observedAt.UTC().UnixNano()))
	copy(key[8:], messageID)
	return key
}
