package bolt

import (
	"context"
	"encoding/json"

	"github.com/xirang-grid/xirang/internal/branding"
	"github.com/xirang-grid/xirang/internal/domain"
	"go.etcd.io/bbolt"
)

func (store *Store) GetClusterBranding(ctx context.Context) (domain.ClusterBranding, error) {
	value := domain.ClusterBranding{Name: "XIRANG"}
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := store.database.View(func(tx *bbolt.Tx) error {
		if data := tx.Bucket(clusterSettingsBucket).Get([]byte("branding")); data != nil {
			return json.Unmarshal(data, &value)
		}
		return nil
	})
	return value, err
}

func (store *Store) PutClusterBranding(ctx context.Context, expected uint64, value domain.ClusterBranding, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(clusterSettingsBucket)
		var current domain.ClusterBranding
		if data := bucket.Get([]byte("branding")); data != nil {
			if err := json.Unmarshal(data, &current); err != nil {
				return err
			}
		}
		if current.Revision != expected {
			return branding.ErrConflict
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte("branding"), data); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}
