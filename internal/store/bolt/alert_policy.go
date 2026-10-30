package bolt

import (
	"context"
	"encoding/json"

	"github.com/xirang-grid/xirang/internal/alertpolicy"
	"github.com/xirang-grid/xirang/internal/domain"
	"go.etcd.io/bbolt"
)

func (store *Store) GetAlertPolicy(ctx context.Context) (domain.AlertPolicy, error) {
	var value domain.AlertPolicy
	if err := ctx.Err(); err != nil {
		return value, err
	}
	err := store.database.View(func(tx *bbolt.Tx) error {
		if data := tx.Bucket(clusterSettingsBucket).Get([]byte("alert-policy")); data != nil {
			return json.Unmarshal(data, &value)
		}
		return nil
	})
	return value, err
}

func (store *Store) PutAlertPolicy(ctx context.Context, expected uint64, value domain.AlertPolicy, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(clusterSettingsBucket)
		var current domain.AlertPolicy
		if data := bucket.Get([]byte("alert-policy")); data != nil {
			if err := json.Unmarshal(data, &current); err != nil {
				return err
			}
		}
		if current.Revision != expected {
			return alertpolicy.ErrConflict
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte("alert-policy"), data); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}
