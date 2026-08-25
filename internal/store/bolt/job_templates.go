package bolt

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/jobtemplate"
	"go.etcd.io/bbolt"
)

func (store *Store) CreateJobTemplate(ctx context.Context, template domain.JobTemplate, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(template)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(jobTemplatesBucket)
		if bucket.Get([]byte(template.ID)) != nil || bucket.Stats().KeyN >= jobtemplate.MaxTemplates {
			return jobtemplate.ErrConflict
		}
		exists, err := jobTemplateNameExists(bucket, template.Name, "")
		if err != nil {
			return err
		}
		if exists {
			return jobtemplate.ErrConflict
		}
		if err := bucket.Put([]byte(template.ID), encoded); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}

func (store *Store) GetJobTemplate(ctx context.Context, id string) (domain.JobTemplate, error) {
	if err := ctx.Err(); err != nil {
		return domain.JobTemplate{}, err
	}
	var template domain.JobTemplate
	err := store.database.View(func(tx *bbolt.Tx) error {
		encoded := tx.Bucket(jobTemplatesBucket).Get([]byte(id))
		if encoded == nil {
			return jobtemplate.ErrNotFound
		}
		return json.Unmarshal(encoded, &template)
	})
	return template, err
}

func (store *Store) ListJobTemplates(ctx context.Context) ([]domain.JobTemplate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	templates := []domain.JobTemplate{}
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(jobTemplatesBucket).ForEach(func(_, encoded []byte) error {
			var template domain.JobTemplate
			if err := json.Unmarshal(encoded, &template); err != nil {
				return err
			}
			templates = append(templates, template)
			return nil
		})
	})
	sort.Slice(templates, func(i, j int) bool {
		if templates[i].Name == templates[j].Name {
			return templates[i].UpdatedAt.After(templates[j].UpdatedAt)
		}
		return templates[i].Name < templates[j].Name
	})
	return templates, err
}

func (store *Store) UpdateJobTemplate(ctx context.Context, id string, expectedRevision uint64, replacement domain.JobTemplate, audit domain.AuditEvent) (domain.JobTemplate, error) {
	if err := ctx.Err(); err != nil {
		return domain.JobTemplate{}, err
	}
	var updated domain.JobTemplate
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(jobTemplatesBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return jobtemplate.ErrNotFound
		}
		var current domain.JobTemplate
		if err := json.Unmarshal(encoded, &current); err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return jobtemplate.ErrConflict
		}
		exists, err := jobTemplateNameExists(bucket, replacement.Name, id)
		if err != nil {
			return err
		}
		if exists {
			return jobtemplate.ErrConflict
		}
		current.Name = replacement.Name
		current.Description = replacement.Description
		current.Script = replacement.Script
		current.UpdatedBy = replacement.UpdatedBy
		current.UpdatedAt = replacement.UpdatedAt
		current.Revision++
		updatedEncoding, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id), updatedEncoding); err != nil {
			return err
		}
		if err := putAudit(tx, audit); err != nil {
			return err
		}
		updated = current
		return nil
	})
	return updated, err
}

func (store *Store) DeleteJobTemplate(ctx context.Context, id string, expectedRevision uint64, audit domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(jobTemplatesBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return jobtemplate.ErrNotFound
		}
		var current domain.JobTemplate
		if err := json.Unmarshal(encoded, &current); err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return jobtemplate.ErrConflict
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		return putAudit(tx, audit)
	})
}

func jobTemplateNameExists(bucket *bbolt.Bucket, name, exceptID string) (bool, error) {
	exists := false
	err := bucket.ForEach(func(key, encoded []byte) error {
		if string(key) == exceptID {
			return nil
		}
		var template domain.JobTemplate
		if err := json.Unmarshal(encoded, &template); err != nil {
			return err
		}
		if strings.EqualFold(template.Name, name) {
			exists = true
		}
		return nil
	})
	return exists, err
}
