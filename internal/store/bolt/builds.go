package bolt

import (
	"context"
	"encoding/json"
	"sort"

	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/internal/domain"
	"go.etcd.io/bbolt"
)

func (store *Store) PutBuild(ctx context.Context, build domain.Build) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(build)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(buildsBucket).Put([]byte(build.ID), encoded)
	})
}

func (store *Store) GetBuild(ctx context.Context, id string) (domain.Build, error) {
	if err := ctx.Err(); err != nil {
		return domain.Build{}, err
	}
	var build domain.Build
	err := store.database.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(buildsBucket).Get([]byte(id))
		if encoded == nil {
			return buildmanager.ErrNotFound
		}
		return json.Unmarshal(encoded, &build)
	})
	return build, err
}

func (store *Store) ListBuilds(ctx context.Context, limit int) ([]domain.Build, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	builds := make([]domain.Build, 0)
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(buildsBucket).ForEach(func(_, encoded []byte) error {
			var build domain.Build
			if err := json.Unmarshal(encoded, &build); err != nil {
				return err
			}
			builds = append(builds, build)
			return nil
		})
	})
	sort.Slice(builds, func(i, j int) bool { return builds[i].CreatedAt.After(builds[j].CreatedAt) })
	if len(builds) > limit {
		builds = builds[:limit]
	}
	return builds, err
}

func (store *Store) DeleteBuild(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(buildsBucket)
		if bucket.Get([]byte(id)) == nil {
			return buildmanager.ErrNotFound
		}
		return bucket.Delete([]byte(id))
	})
}

func (store *Store) GetBuildRetentionPolicy(ctx context.Context) (domain.BuildRetentionPolicy, error) {
	if err := ctx.Err(); err != nil {
		return domain.BuildRetentionPolicy{}, err
	}
	var policy domain.BuildRetentionPolicy
	err := store.database.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(buildSettingsBucket).Get([]byte("retention"))
		if encoded == nil {
			return buildmanager.ErrNotFound
		}
		return json.Unmarshal(encoded, &policy)
	})
	return policy, err
}

func (store *Store) PutBuildRetentionPolicy(ctx context.Context, policy domain.BuildRetentionPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(buildSettingsBucket).Put([]byte("retention"), encoded)
	})
}
