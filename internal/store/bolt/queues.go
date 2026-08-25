package bolt

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/xirang-grid/xirang/internal/domain"
	"go.etcd.io/bbolt"
)

func (store *Store) PutQueuePolicy(ctx context.Context, policy domain.QueuePolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(queuePoliciesBucket).Put([]byte(policy.Name), encoded)
	})
}

func (store *Store) ListQueuePolicies(ctx context.Context) ([]domain.QueuePolicy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	policies := make([]domain.QueuePolicy, 0)
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(queuePoliciesBucket).ForEach(func(_, encoded []byte) error {
			var policy domain.QueuePolicy
			if err := json.Unmarshal(encoded, &policy); err != nil {
				return err
			}
			policies = append(policies, policy)
			return nil
		})
	})
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	return policies, err
}
