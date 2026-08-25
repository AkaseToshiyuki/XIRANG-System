package bolt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	"go.etcd.io/bbolt"
)

func (store *Store) CreateAPIToken(ctx context.Context, token domain.APIToken, digest [sha256.Size]byte, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(token)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		if tx.Bucket(apiTokensBucket).Get([]byte(token.ID)) != nil {
			return identity.ErrConflict
		}
		digestKey := []byte(hex.EncodeToString(digest[:]))
		if tx.Bucket(apiTokenDigestsBucket).Get(digestKey) != nil {
			return identity.ErrConflict
		}
		if err := tx.Bucket(apiTokensBucket).Put([]byte(token.ID), encoded); err != nil {
			return err
		}
		if err := tx.Bucket(apiTokenDigestsBucket).Put(digestKey, []byte(token.ID)); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}

func (store *Store) GetAPITokenByDigest(ctx context.Context, digest [sha256.Size]byte) (domain.APIToken, error) {
	if err := ctx.Err(); err != nil {
		return domain.APIToken{}, err
	}
	var token domain.APIToken
	err := store.database.View(func(tx *bbolt.Tx) error {
		id := tx.Bucket(apiTokenDigestsBucket).Get([]byte(hex.EncodeToString(digest[:])))
		if id == nil {
			return identity.ErrNotFound
		}
		encoded := tx.Bucket(apiTokensBucket).Get(id)
		if encoded == nil {
			return identity.ErrNotFound
		}
		return json.Unmarshal(encoded, &token)
	})
	return token, err
}

func (store *Store) ListAPITokens(ctx context.Context) ([]domain.APIToken, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tokens := make([]domain.APIToken, 0)
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(apiTokensBucket).ForEach(func(_, encoded []byte) error {
			var token domain.APIToken
			if err := json.Unmarshal(encoded, &token); err != nil {
				return err
			}
			tokens = append(tokens, token)
			return nil
		})
	})
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].CreatedAt.After(tokens[j].CreatedAt) })
	return tokens, err
}

func (store *Store) UpdateAPIToken(ctx context.Context, id string, update func(*domain.APIToken) error, audit *domain.AuditEvent) (domain.APIToken, error) {
	if err := ctx.Err(); err != nil {
		return domain.APIToken{}, err
	}
	var updated domain.APIToken
	err := store.database.Update(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(apiTokensBucket).Get([]byte(id))
		if encoded == nil {
			return identity.ErrNotFound
		}
		var token domain.APIToken
		if err := json.Unmarshal(encoded, &token); err != nil {
			return err
		}
		if err := update(&token); err != nil {
			return err
		}
		encoded, err := json.Marshal(token)
		if err != nil {
			return err
		}
		if err := tx.Bucket(apiTokensBucket).Put([]byte(id), encoded); err != nil {
			return err
		}
		if audit != nil {
			if err := putAudit(tx, *audit); err != nil {
				return err
			}
		}
		updated = token
		return nil
	})
	return updated, err
}
