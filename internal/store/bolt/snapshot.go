package bolt

import (
	"context"
	"io"

	"go.etcd.io/bbolt"
)

func (store *Store) Snapshot(ctx context.Context, writer io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.View(func(tx *bbolt.Tx) error { _, err := tx.WriteTo(writer); return err })
}
