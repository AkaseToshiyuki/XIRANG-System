package bolt

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	"go.etcd.io/bbolt"
)

func (store *Store) GetAccessSettings(ctx context.Context) (domain.AccessSettings, error) {
	if err := ctx.Err(); err != nil {
		return domain.AccessSettings{}, err
	}
	var settings domain.AccessSettings
	err := store.database.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(accessSettingsBucket).Get([]byte("cluster"))
		if encoded == nil {
			return nil
		}
		return json.Unmarshal(encoded, &settings)
	})
	return settings, err
}

func (store *Store) PutAccessSettings(ctx context.Context, settings domain.AccessSettings, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		if settings.DefaultUserGroupID != "" && tx.Bucket(userGroupsBucket).Get([]byte(settings.DefaultUserGroupID)) == nil {
			return identity.ErrNotFound
		}
		if err := tx.Bucket(accessSettingsBucket).Put([]byte("cluster"), encoded); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}

func (store *Store) CreateUserGroup(ctx context.Context, group domain.UserGroup, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(group)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(userGroupsBucket)
		if bucket.Get([]byte(group.ID)) != nil {
			return identity.ErrConflict
		}
		if err := bucket.Put([]byte(group.ID), encoded); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}

func (store *Store) GetUserGroup(ctx context.Context, id string) (domain.UserGroup, error) {
	if err := ctx.Err(); err != nil {
		return domain.UserGroup{}, err
	}
	var group domain.UserGroup
	err := store.database.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(userGroupsBucket).Get([]byte(id))
		if encoded == nil {
			return identity.ErrNotFound
		}
		return json.Unmarshal(encoded, &group)
	})
	return group, err
}

func (store *Store) ListUserGroups(ctx context.Context) ([]domain.UserGroup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	groups := []domain.UserGroup{}
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(userGroupsBucket).ForEach(func(_, encoded []byte) error {
			var group domain.UserGroup
			if err := json.Unmarshal(encoded, &group); err != nil {
				return err
			}
			groups = append(groups, group)
			return nil
		})
	})
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups, err
}

func (store *Store) UpdateUserGroup(ctx context.Context, id string, update func(*domain.UserGroup) error, audit domain.AuditEvent) (domain.UserGroup, error) {
	if err := ctx.Err(); err != nil {
		return domain.UserGroup{}, err
	}
	var result domain.UserGroup
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(userGroupsBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return identity.ErrNotFound
		}
		var group domain.UserGroup
		if err := json.Unmarshal(encoded, &group); err != nil {
			return err
		}
		if err := update(&group); err != nil {
			return err
		}
		encoded, err := json.Marshal(group)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id), encoded); err != nil {
			return err
		}
		if err := putAudit(tx, audit); err != nil {
			return err
		}
		result = group
		return nil
	})
	return result, err
}

func (store *Store) DeleteUserGroup(ctx context.Context, id string, audit domain.AuditEvent) ([]domain.User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	affected := []domain.User{}
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(userGroupsBucket)
		if bucket.Get([]byte(id)) == nil {
			return identity.ErrNotFound
		}
		type userUpdate struct {
			key    []byte
			record []byte
		}
		updates := []userUpdate{}
		if err := tx.Bucket(usersBucket).ForEach(func(key, encoded []byte) error {
			var record storedUser
			if err := json.Unmarshal(encoded, &record); err != nil {
				return err
			}
			groups := make([]string, 0, len(record.User.GroupIDs))
			matched := false
			for _, groupID := range record.User.GroupIDs {
				if groupID == id {
					matched = true
					continue
				}
				groups = append(groups, groupID)
			}
			if !matched {
				return nil
			}
			record.User.GroupIDs = groups
			record.User.UpdatedAt = audit.OccurredAt
			updated, err := json.Marshal(record)
			if err != nil {
				return err
			}
			updates = append(updates, userUpdate{key: append([]byte(nil), key...), record: updated})
			affected = append(affected, record.User)
			return nil
		}); err != nil {
			return err
		}
		for _, update := range updates {
			if err := tx.Bucket(usersBucket).Put(update.key, update.record); err != nil {
				return err
			}
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		defaultCleared := false
		settingsBucket := tx.Bucket(accessSettingsBucket)
		if encoded := settingsBucket.Get([]byte("cluster")); encoded != nil {
			var settings domain.AccessSettings
			if err := json.Unmarshal(encoded, &settings); err != nil {
				return err
			}
			if settings.DefaultUserGroupID == id {
				settings.DefaultUserGroupID = ""
				settings.UpdatedAt = audit.OccurredAt
				updated, err := json.Marshal(settings)
				if err != nil {
					return err
				}
				if err := settingsBucket.Put([]byte("cluster"), updated); err != nil {
					return err
				}
				defaultCleared = true
			}
		}
		if audit.Details == nil {
			audit.Details = map[string]string{}
		}
		audit.Details["detachedUsers"] = strconv.Itoa(len(affected))
		audit.Details["defaultCleared"] = strconv.FormatBool(defaultCleared)
		return putAudit(tx, audit)
	})
	return affected, err
}
