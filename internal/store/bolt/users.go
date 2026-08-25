package bolt

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	"go.etcd.io/bbolt"
)

type storedUser struct {
	User         domain.User `json:"user"`
	PasswordHash string      `json:"passwordHash"`
}

func (store *Store) HasUsers(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var found bool
	err := store.database.View(func(tx *bbolt.Tx) error {
		key, _ := tx.Bucket(usersBucket).Cursor().First()
		found = key != nil
		return nil
	})
	return found, err
}

func (store *Store) CreateFirstUser(ctx context.Context, user domain.User, passwordHash string, audit domain.AuditEvent) error {
	_, err := store.createUser(ctx, user, passwordHash, audit, true)
	return err
}

func (store *Store) CreateUser(ctx context.Context, user domain.User, passwordHash string, audit domain.AuditEvent) (domain.User, error) {
	return store.createUser(ctx, user, passwordHash, audit, false)
}

func (store *Store) createUser(ctx context.Context, user domain.User, passwordHash string, audit domain.AuditEvent, requireEmpty bool) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	err := store.database.Update(func(tx *bbolt.Tx) error {
		users, names := tx.Bucket(usersBucket), tx.Bucket(usernamesBucket)
		if requireEmpty {
			key, _ := users.Cursor().First()
			if key != nil {
				return identity.ErrConflict
			}
		}
		nameKey := []byte(strings.ToLower(user.Username))
		if users.Get([]byte(user.ID)) != nil || names.Get(nameKey) != nil {
			return identity.ErrConflict
		}
		if !requireEmpty {
			var settings domain.AccessSettings
			if encoded := tx.Bucket(accessSettingsBucket).Get([]byte("cluster")); encoded != nil {
				if err := json.Unmarshal(encoded, &settings); err != nil {
					return err
				}
			}
			if settings.DefaultUserGroupID != "" {
				if tx.Bucket(userGroupsBucket).Get([]byte(settings.DefaultUserGroupID)) == nil {
					return identity.ErrNotFound
				}
				user.GroupIDs = []string{settings.DefaultUserGroupID}
			}
		}
		record, err := json.Marshal(storedUser{User: user, PasswordHash: passwordHash})
		if err != nil {
			return err
		}
		if err := users.Put([]byte(user.ID), record); err != nil {
			return err
		}
		if err := names.Put(nameKey, []byte(user.ID)); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
	return user, err
}

func (store *Store) GetUser(ctx context.Context, id string) (domain.User, string, error) {
	return store.readUser(ctx, func(tx *bbolt.Tx) []byte { return []byte(id) })
}

func (store *Store) GetUserByUsername(ctx context.Context, username string) (domain.User, string, error) {
	return store.readUser(ctx, func(tx *bbolt.Tx) []byte {
		id := tx.Bucket(usernamesBucket).Get([]byte(strings.ToLower(strings.TrimSpace(username))))
		return append([]byte(nil), id...)
	})
}

func (store *Store) readUser(ctx context.Context, id func(*bbolt.Tx) []byte) (domain.User, string, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, "", err
	}
	var record storedUser
	err := store.database.View(func(tx *bbolt.Tx) error {
		key := id(tx)
		if len(key) == 0 {
			return identity.ErrNotFound
		}
		encoded := tx.Bucket(usersBucket).Get(key)
		if encoded == nil {
			return identity.ErrNotFound
		}
		return json.Unmarshal(encoded, &record)
	})
	return record.User, record.PasswordHash, err
}

func (store *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	users := []domain.User{}
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(usersBucket).ForEach(func(_, encoded []byte) error {
			var record storedUser
			if err := json.Unmarshal(encoded, &record); err != nil {
				return err
			}
			users = append(users, record.User)
			return nil
		})
	})
	sort.Slice(users, func(i, j int) bool { return users[i].CreatedAt.Before(users[j].CreatedAt) })
	return users, err
}

func (store *Store) UpdateUser(ctx context.Context, id string, update func(*domain.User) error, audit domain.AuditEvent) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	var result domain.User
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(usersBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return identity.ErrNotFound
		}
		var record storedUser
		if err := json.Unmarshal(encoded, &record); err != nil {
			return err
		}
		if err := update(&record.User); err != nil {
			return err
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id), encoded); err != nil {
			return err
		}
		if err := putAudit(tx, audit); err != nil {
			return err
		}
		result = record.User
		return nil
	})
	return result, err
}
