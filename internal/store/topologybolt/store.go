package topologybolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/topology"
	"go.etcd.io/bbolt"
)

var (
	reportsBucket   = []byte("reports")
	snapshotsBucket = []byte("snapshots")
	metadataBucket  = []byte("metadata")
	latestKey       = []byte("latest")
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
	if err := database.Update(func(transaction *bbolt.Tx) error {
		for _, bucket := range [][]byte{reportsBucket, snapshotsBucket, metadataBucket} {
			if _, err := transaction.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error { return store.database.Close() }

func (store *Store) ApplyReport(ctx context.Context, report domain.NodeTopologyReport, builder topology.SnapshotBuilder) (domain.TopologySnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.TopologySnapshot{}, false, err
	}
	var snapshot domain.TopologySnapshot
	changed := false
	err := store.database.Update(func(transaction *bbolt.Tx) error {
		reports := transaction.Bucket(reportsBucket)
		if encoded := reports.Get([]byte(report.NodeID)); encoded != nil {
			var previous domain.NodeTopologyReport
			if err := json.Unmarshal(encoded, &previous); err != nil {
				return err
			}
			if previous.ContentHash == report.ContentHash {
				latest, err := latestSnapshot(transaction)
				snapshot = latest
				return err
			}
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			return err
		}
		if err := reports.Put([]byte(report.NodeID), encoded); err != nil {
			return err
		}
		allReports := make([]domain.NodeTopologyReport, 0, reports.Stats().KeyN+1)
		if err := reports.ForEach(func(_, value []byte) error {
			var item domain.NodeTopologyReport
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			allReports = append(allReports, item)
			return nil
		}); err != nil {
			return err
		}
		snapshots := transaction.Bucket(snapshotsBucket)
		version, err := snapshots.NextSequence()
		if err != nil {
			return err
		}
		snapshot = builder(allReports, version, time.Now())
		encodedSnapshot, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		key := versionKey(version)
		if err := snapshots.Put(key, encodedSnapshot); err != nil {
			return err
		}
		if err := transaction.Bucket(metadataBucket).Put(latestKey, key); err != nil {
			return err
		}
		keyCount := snapshots.Stats().KeyN + 1
		for keyCount > store.retention {
			oldest, _ := snapshots.Cursor().First()
			if oldest == nil {
				break
			}
			if err := snapshots.Delete(oldest); err != nil {
				return err
			}
			keyCount--
		}
		changed = true
		return nil
	})
	return snapshot, changed, err
}

func (store *Store) LatestSnapshot(ctx context.Context) (domain.TopologySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.TopologySnapshot{}, err
	}
	var snapshot domain.TopologySnapshot
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		snapshot, err = latestSnapshot(transaction)
		return err
	})
	return snapshot, err
}

func (store *Store) GetSnapshot(ctx context.Context, version uint64) (domain.TopologySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.TopologySnapshot{}, err
	}
	var snapshot domain.TopologySnapshot
	err := store.database.View(func(transaction *bbolt.Tx) error {
		encoded := transaction.Bucket(snapshotsBucket).Get(versionKey(version))
		if encoded == nil {
			return topology.ErrNotFound
		}
		return json.Unmarshal(encoded, &snapshot)
	})
	return snapshot, err
}

func latestSnapshot(transaction *bbolt.Tx) (domain.TopologySnapshot, error) {
	key := transaction.Bucket(metadataBucket).Get(latestKey)
	if key == nil {
		return domain.TopologySnapshot{}, topology.ErrNotFound
	}
	encoded := transaction.Bucket(snapshotsBucket).Get(key)
	if encoded == nil {
		return domain.TopologySnapshot{}, topology.ErrNotFound
	}
	var snapshot domain.TopologySnapshot
	return snapshot, json.Unmarshal(encoded, &snapshot)
}

func versionKey(version uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, version)
	return key
}
