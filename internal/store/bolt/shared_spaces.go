package bolt

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"go.etcd.io/bbolt"
)

func (store *Store) PutSharedSpace(ctx context.Context, space domain.SharedSpace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(space)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(sharedSpacesBucket).Put([]byte(space.ID), encoded)
	})
}

func (store *Store) GetSharedSpace(ctx context.Context, id string) (domain.SharedSpace, error) {
	if err := ctx.Err(); err != nil {
		return domain.SharedSpace{}, err
	}
	var result domain.SharedSpace
	err := store.database.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(sharedSpacesBucket).Get([]byte(id))
		if encoded == nil {
			return enrollment.ErrNotFound
		}
		return json.Unmarshal(encoded, &result)
	})
	return result, err
}

func (store *Store) ListSharedSpaces(ctx context.Context) ([]domain.SharedSpace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := []domain.SharedSpace{}
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(sharedSpacesBucket).ForEach(func(_, encoded []byte) error {
			var space domain.SharedSpace
			if err := json.Unmarshal(encoded, &space); err != nil {
				return err
			}
			result = append(result, space)
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result, err
}

func (store *Store) DeleteSharedSpace(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(sharedSpacesBucket)
		if bucket.Get([]byte(id)) == nil {
			return enrollment.ErrNotFound
		}
		return bucket.Delete([]byte(id))
	})
}
