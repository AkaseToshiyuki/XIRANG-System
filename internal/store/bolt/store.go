package bolt

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
)

var (
	tokensBucket              = []byte("enrollment_tokens")
	nodesBucket               = []byte("nodes")
	auditBucket               = []byte("audit_events")
	assignmentsBucket         = []byte("assignment_reports")
	jobsBucket                = []byte("jobs")
	jobsByOwnerBucket         = []byte("jobs_by_owner")
	activeJobsBucket          = []byte("jobs_active")
	jobsFinishedBucket        = []byte("jobs_finished")
	jobAssignmentsBucket      = []byte("job_assignments")
	queuePoliciesBucket       = []byte("queue_policies")
	discoveryScansBucket      = []byte("discovery_scans")
	discoveryCandidatesBucket = []byte("discovery_candidates")
	buildsBucket              = []byte("builds")
	buildSettingsBucket       = []byte("build_settings")
	apiTokensBucket           = []byte("api_tokens")
	apiTokenDigestsBucket     = []byte("api_token_digests")
	usersBucket               = []byte("users")
	usernamesBucket           = []byte("usernames")
	userGroupsBucket          = []byte("user_groups")
	accessSettingsBucket      = []byte("access_settings")
	clusterSettingsBucket     = []byte("cluster_settings")
	hostHealthBucket          = []byte("host_health")
	systemEventsBucket        = []byte("system_events")
	bmcActionsBucket          = []byte("bmc_actions")
	sharedSpacesBucket        = []byte("shared_spaces")
	jobTemplatesBucket        = []byte("job_templates")
	usageDailyBucket          = []byte("usage_daily")
	billingRatesBucket        = []byte("billing_rates")
	schemaMetadataBucket      = []byte("schema_metadata")
)

const currentSchemaVersion = "1"

type Store struct {
	database *bbolt.DB
}

type tokenRecord struct {
	Token      domain.EnrollmentToken `json:"token"`
	SecretHash string                 `json:"secretHash"`
}

type nodeRecord struct {
	Node domain.Node `json:"node"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	database, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	store := &Store{database: database}
	if err := database.Update(func(transaction *bbolt.Tx) error {
		if _, err := transaction.CreateBucketIfNotExists(tokensBucket); err != nil {
			return err
		}
		if _, err := transaction.CreateBucketIfNotExists(nodesBucket); err != nil {
			return err
		}
		if _, err := transaction.CreateBucketIfNotExists(auditBucket); err != nil {
			return err
		}
		for _, bucket := range [][]byte{
			assignmentsBucket, jobsBucket, jobsByOwnerBucket, activeJobsBucket, jobsFinishedBucket, jobAssignmentsBucket, queuePoliciesBucket,
			discoveryScansBucket, discoveryCandidatesBucket,
			buildsBucket, buildSettingsBucket, apiTokensBucket, apiTokenDigestsBucket, usersBucket, usernamesBucket,
			userGroupsBucket, accessSettingsBucket, clusterSettingsBucket, hostHealthBucket, systemEventsBucket, bmcActionsBucket, sharedSpacesBucket, jobTemplatesBucket, schemaMetadataBucket,
			usageDailyBucket, billingRatesBucket,
		} {
			if _, err := transaction.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		metadata := transaction.Bucket(schemaMetadataBucket)
		version := metadata.Get([]byte("version"))
		if version == nil {
			if err := metadata.Put([]byte("version"), []byte(currentSchemaVersion)); err != nil {
				return err
			}
			return backfillJobIndexes(transaction)
		}
		if string(version) != currentSchemaVersion {
			return errors.New("unsupported control-plane schema version " + string(version))
		}
		return backfillJobIndexes(transaction)
	}); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error { return store.database.Close() }

func (store *Store) CreateEnrollmentToken(ctx context.Context, token domain.EnrollmentToken) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record := tokenRecord{Token: token, SecretHash: token.SecretHash}
	record.Token.SecretHash = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(tokensBucket)
		if bucket.Get([]byte(token.ID)) != nil {
			return enrollment.ErrConflict
		}
		return bucket.Put([]byte(token.ID), encoded)
	})
}

func (store *Store) RevokeEnrollmentToken(ctx context.Context, id string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(tokensBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return enrollment.ErrNotFound
		}
		var record tokenRecord
		if err := json.Unmarshal(encoded, &record); err != nil {
			return err
		}
		record.Token.RevokedAt = &now
		updated, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(id), updated)
	})
}

func (store *Store) EnrollNode(ctx context.Context, tokenID, secretHash string, node domain.Node, now time.Time) (domain.EnrollmentToken, error) {
	if err := ctx.Err(); err != nil {
		return domain.EnrollmentToken{}, err
	}
	var consumed domain.EnrollmentToken
	err := store.database.Update(func(transaction *bbolt.Tx) error {
		tokenBucket := transaction.Bucket(tokensBucket)
		nodeBucket := transaction.Bucket(nodesBucket)
		encodedToken := tokenBucket.Get([]byte(tokenID))
		if encodedToken == nil {
			return enrollment.ErrInvalidToken
		}
		var record tokenRecord
		if err := json.Unmarshal(encodedToken, &record); err != nil {
			return err
		}
		if !record.Token.Available(now) || subtle.ConstantTimeCompare([]byte(record.SecretHash), []byte(secretHash)) != 1 {
			return enrollment.ErrInvalidToken
		}
		if nodeBucket.Get([]byte(node.ID)) != nil {
			return enrollment.ErrConflict
		}
		record.Token.Uses++
		consumed = record.Token
		updatedToken, err := json.Marshal(record)
		if err != nil {
			return err
		}
		encodedNode, err := json.Marshal(nodeRecord{Node: node})
		if err != nil {
			return err
		}
		if err := tokenBucket.Put([]byte(tokenID), updatedToken); err != nil {
			return err
		}
		return nodeBucket.Put([]byte(node.ID), encodedNode)
	})
	return consumed, err
}

func (store *Store) GetNode(ctx context.Context, id string) (domain.Node, error) {
	if err := ctx.Err(); err != nil {
		return domain.Node{}, err
	}
	var node domain.Node
	err := store.database.View(func(transaction *bbolt.Tx) error {
		encoded := transaction.Bucket(nodesBucket).Get([]byte(id))
		if encoded == nil {
			return enrollment.ErrNotFound
		}
		var record nodeRecord
		if err := json.Unmarshal(encoded, &record); err != nil {
			return err
		}
		node = record.Node
		return nil
	})
	return node, err
}

func (store *Store) ListNodes(ctx context.Context) ([]domain.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nodes := []domain.Node{}
	err := store.database.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(nodesBucket).ForEach(func(_, encoded []byte) error {
			var record nodeRecord
			if err := json.Unmarshal(encoded, &record); err != nil {
				return err
			}
			nodes = append(nodes, record.Node)
			return nil
		})
	})
	sort.Slice(nodes, func(left, right int) bool {
		return nodes[left].CreatedAt.Before(nodes[right].CreatedAt)
	})
	return nodes, err
}

func (store *Store) UpdateNode(ctx context.Context, id string, update func(*domain.Node) error, audit *domain.AuditEvent) (domain.Node, error) {
	if err := ctx.Err(); err != nil {
		return domain.Node{}, err
	}
	var updated domain.Node
	err := store.database.Update(func(transaction *bbolt.Tx) error {
		bucket := transaction.Bucket(nodesBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return enrollment.ErrNotFound
		}
		var record nodeRecord
		if err := json.Unmarshal(encoded, &record); err != nil {
			return err
		}
		if err := update(&record.Node); err != nil {
			return err
		}
		record.Node.Revision++
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id), encoded); err != nil {
			return err
		}
		if audit != nil {
			encodedAudit, err := json.Marshal(audit)
			if err != nil {
				return err
			}
			key := audit.OccurredAt.UTC().Format("20060102T150405.000000000Z") + "_" + audit.ID
			if err := transaction.Bucket(auditBucket).Put([]byte(key), encodedAudit); err != nil {
				return err
			}
		}
		updated = record.Node
		return nil
	})
	return updated, err
}

func (store *Store) ListAuditEvents(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	events := make([]domain.AuditEvent, 0, limit)
	err := store.database.View(func(transaction *bbolt.Tx) error {
		cursor := transaction.Bucket(auditBucket).Cursor()
		for key, encoded := cursor.Last(); key != nil && len(events) < limit; key, encoded = cursor.Prev() {
			var event domain.AuditEvent
			if err := json.Unmarshal(encoded, &event); err != nil {
				return err
			}
			events = append(events, event)
		}
		return nil
	})
	return events, err
}

func (store *Store) AppendAuditEvent(ctx context.Context, event domain.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return putAudit(transaction, event)
	})
}

func (store *Store) UpsertAssignmentReport(ctx context.Context, report domain.AssignmentReport) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		_, err := upsertAssignmentReport(transaction, report)
		return err
	})
}

func (store *Store) ListAssignmentReports(ctx context.Context, limit int) ([]domain.AssignmentReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	reports := make([]domain.AssignmentReport, 0, limit)
	err := store.database.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(assignmentsBucket).ForEach(func(_, encoded []byte) error {
			var report domain.AssignmentReport
			if err := json.Unmarshal(encoded, &report); err != nil {
				return err
			}
			reports = append(reports, report)
			return nil
		})
	})
	sort.Slice(reports, func(left, right int) bool { return reports[left].ReceivedAt.After(reports[right].ReceivedAt) })
	if len(reports) > limit {
		reports = reports[:limit]
	}
	return reports, err
}

func IsDatabaseLocked(err error) bool { return errors.Is(err, bbolt.ErrTimeout) }
