package flowbolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"go.etcd.io/bbolt"
)

var (
	latestBucket  = []byte("latest")
	historyBucket = []byte("history")
)

const defaultRetention = 120

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
	if err := database.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(latestBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(historyBucket)
		return err
	}); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error { return store.database.Close() }

func (store *Store) Append(ctx context.Context, observation domain.NodeFlowObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(latestBucket).Put([]byte(observation.NodeID), encoded); err != nil {
			return err
		}
		node, err := tx.Bucket(historyBucket).CreateBucketIfNotExists([]byte(observation.NodeID))
		if err != nil {
			return err
		}
		sequence, err := node.NextSequence()
		if err != nil {
			return err
		}
		if err := node.Put(sequenceKey(sequence), encoded); err != nil {
			return err
		}
		if sequence > uint64(store.retention) {
			if err := node.Delete(sequenceKey(sequence - uint64(store.retention))); err != nil {
				return err
			}
		}
		return nil
	})
}

func (store *Store) List(ctx context.Context, nodeID string, limit int) ([]domain.NodeFlowObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items := make([]domain.NodeFlowObservation, 0, limit)
	err := store.database.View(func(tx *bbolt.Tx) error {
		node := tx.Bucket(historyBucket).Bucket([]byte(nodeID))
		if node == nil {
			return nil
		}
		cursor := node.Cursor()
		for key, value := cursor.Last(); key != nil && len(items) < limit; key, value = cursor.Prev() {
			var item domain.NodeFlowObservation
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			items = append(items, item)
		}
		return nil
	})
	return items, err
}

func (store *Store) ListLatest(ctx context.Context) ([]domain.NodeFlowObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items := make([]domain.NodeFlowObservation, 0)
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(latestBucket).ForEach(func(_, value []byte) error {
			var item domain.NodeFlowObservation
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			items = append(items, item)
			return nil
		})
	})
	sort.Slice(items, func(i, j int) bool { return items[i].NodeID < items[j].NodeID })
	return items, err
}

func sequenceKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}
