// Package external provides a PostgreSQL-backed active/passive control-state
// adapter. The active process holds a session advisory lock and writes through
// every durable mutation as a transactionally replaced, structurally valid bbolt
// image. A successor downloads the last committed image before serving.
package external

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/xirang-grid/xirang/internal/domain"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

type Config struct {
	DSN       string
	Namespace string
	CachePath string
	Standby   bool
	driver    string
}

type Store struct {
	*boltstore.Store
	database  *sql.DB
	lockConn  *sql.Conn
	namespace string
	lockKey   int64
	leaderPID int64
	cachePath string
	syncMu    sync.Mutex
}

func Open(ctx context.Context, config Config, onStandby func()) (*Store, error) {
	if config.DSN == "" || config.Namespace == "" || config.CachePath == "" || !filepath.IsAbs(config.CachePath) {
		return nil, errors.New("external state requires DSN, namespace, and absolute cache path")
	}
	parsedDSN, err := url.Parse(config.DSN)
	if err != nil || (parsedDSN.Scheme != "postgres" && parsedDSN.Scheme != "postgresql") || parsedDSN.Hostname() == "" {
		return nil, errors.New("external state DSN must be a PostgreSQL URL")
	}
	if parsedDSN.Query().Get("sslmode") == "disable" && !isLoopbackHost(parsedDSN.Hostname()) {
		return nil, errors.New("external PostgreSQL transport cannot disable TLS for a remote host")
	}
	driverName := config.driver
	if driverName == "" {
		driverName = "pgx"
	}
	database, err := sql.Open(driverName, config.DSN)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(2)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS xirang_control_state (
		namespace TEXT PRIMARY KEY,
		schema_version INTEGER NOT NULL,
		image BYTEA NOT NULL,
		image_sha256 BYTEA NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`); err != nil {
		_ = database.Close()
		return nil, err
	}
	lockConn, err := database.Conn(ctx)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte("xirang:" + config.Namespace))
	lockKey := int64(hasher.Sum64())
	for {
		var acquired bool
		if err := lockConn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&acquired); err != nil {
			_ = lockConn.Close()
			_ = database.Close()
			return nil, err
		}
		if acquired {
			break
		}
		if !config.Standby {
			_ = lockConn.Close()
			_ = database.Close()
			return nil, errors.New("another XIRANG controller holds the external-state leadership lock")
		}
		if onStandby != nil {
			onStandby()
		}
		select {
		case <-ctx.Done():
			_ = lockConn.Close()
			_ = database.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	var leaderPID int64
	if err := lockConn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&leaderPID); err != nil || leaderPID <= 0 {
		_ = lockConn.Close()
		_ = database.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("external-state leadership backend identity is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(config.CachePath), 0o700); err != nil {
		_ = lockConn.Close()
		_ = database.Close()
		return nil, err
	}
	var image, expectedDigest []byte
	var schemaVersion int
	err = database.QueryRowContext(ctx, `SELECT schema_version,image,image_sha256 FROM xirang_control_state WHERE namespace=$1`, config.Namespace).Scan(&schemaVersion, &image, &expectedDigest)
	switch {
	case err == nil:
		if schemaVersion != 1 {
			_ = lockConn.Close()
			_ = database.Close()
			return nil, errors.New("external control-state schema is newer or unsupported")
		}
		actual := sha256.Sum256(image)
		if !bytes.Equal(actual[:], expectedDigest) {
			_ = lockConn.Close()
			_ = database.Close()
			return nil, errors.New("external control-state image checksum mismatch")
		}
		temporary := config.CachePath + ".download.tmp"
		if err := os.WriteFile(temporary, image, 0o600); err != nil {
			_ = lockConn.Close()
			_ = database.Close()
			return nil, err
		}
		if err := os.Rename(temporary, config.CachePath); err != nil {
			_ = lockConn.Close()
			_ = database.Close()
			return nil, err
		}
	case errors.Is(err, sql.ErrNoRows):
		_ = os.Remove(config.CachePath)
	default:
		_ = lockConn.Close()
		_ = database.Close()
		return nil, err
	}
	local, err := boltstore.Open(config.CachePath)
	if err != nil {
		_ = lockConn.Close()
		_ = database.Close()
		return nil, err
	}
	store := &Store{
		Store: local, database: database, lockConn: lockConn, namespace: config.Namespace,
		lockKey: lockKey, leaderPID: leaderPID, cachePath: config.CachePath,
	}
	if len(image) == 0 {
		if err := store.sync(ctx); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	return store, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (store *Store) sync(ctx context.Context) error {
	store.syncMu.Lock()
	defer store.syncMu.Unlock()
	var backendPID int64
	if err := store.lockConn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		return fmt.Errorf("external-state leadership session is unavailable: %w", err)
	}
	if backendPID != store.leaderPID {
		return errors.New("external-state leadership session changed")
	}
	var image bytes.Buffer
	if err := store.Store.Snapshot(ctx, &image); err != nil {
		return err
	}
	digest := sha256.Sum256(image.Bytes())
	_, err := store.database.ExecContext(ctx, `INSERT INTO xirang_control_state(namespace,schema_version,image,image_sha256,updated_at)
		VALUES($1,1,$2,$3,NOW())
		ON CONFLICT(namespace) DO UPDATE SET schema_version=EXCLUDED.schema_version,image=EXCLUDED.image,image_sha256=EXCLUDED.image_sha256,updated_at=EXCLUDED.updated_at`,
		store.namespace, image.Bytes(), digest[:])
	return err
}

func afterCall[T any](call func() (T, error), sync func() error) (T, error) {
	value, err := call()
	if err != nil {
		return value, err
	}
	if err := sync(); err != nil {
		return value, err
	}
	return value, nil
}
func afterBoolCall[T any](call func() (T, bool, error), sync func() error) (T, bool, error) {
	value, flag, err := call()
	if err != nil {
		return value, flag, err
	}
	if err := sync(); err != nil {
		return value, flag, err
	}
	return value, flag, nil
}
func (store *Store) persist(ctx context.Context) error { return store.sync(context.WithoutCancel(ctx)) }

func (store *Store) CreateEnrollmentToken(ctx context.Context, token domain.EnrollmentToken) error {
	if err := store.Store.CreateEnrollmentToken(ctx, token); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) RevokeEnrollmentToken(ctx context.Context, id string, now time.Time) error {
	if err := store.Store.RevokeEnrollmentToken(ctx, id, now); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) EnrollNode(ctx context.Context, tokenID, digest string, node domain.Node, now time.Time) (domain.EnrollmentToken, error) {
	return afterCall(func() (domain.EnrollmentToken, error) {
		return store.Store.EnrollNode(ctx, tokenID, digest, node, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) UpdateNode(ctx context.Context, id string, update func(*domain.Node) error, audit *domain.AuditEvent) (domain.Node, error) {
	return afterCall(func() (domain.Node, error) {
		return store.Store.UpdateNode(ctx, id, update, audit)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) PutSharedSpace(ctx context.Context, space domain.SharedSpace) error {
	if err := store.Store.PutSharedSpace(ctx, space); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) DeleteSharedSpace(ctx context.Context, id string) error {
	if err := store.Store.DeleteSharedSpace(ctx, id); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) CreateJobTemplate(ctx context.Context, template domain.JobTemplate, audit domain.AuditEvent) error {
	if err := store.Store.CreateJobTemplate(ctx, template, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) UpdateJobTemplate(ctx context.Context, id string, revision uint64, template domain.JobTemplate, audit domain.AuditEvent) (domain.JobTemplate, error) {
	return afterCall(func() (domain.JobTemplate, error) {
		return store.Store.UpdateJobTemplate(ctx, id, revision, template, audit)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) DeleteJobTemplate(ctx context.Context, id string, revision uint64, audit domain.AuditEvent) error {
	if err := store.Store.DeleteJobTemplate(ctx, id, revision, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) UpsertAssignmentReport(ctx context.Context, report domain.AssignmentReport) error {
	if err := store.Store.UpsertAssignmentReport(ctx, report); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) CreateJob(ctx context.Context, job domain.Job) error {
	if err := store.Store.CreateJob(ctx, job); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) AssignJob(ctx context.Context, jobID, nodeID, assignmentID string, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.AssignJob(ctx, jobID, nodeID, assignmentID, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) AssignJobAllocation(ctx context.Context, jobID string, allocation domain.Allocation, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.AssignJobAllocation(ctx, jobID, allocation, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) RollbackJobAllocation(ctx context.Context, jobID, allocationID, reason string, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.RollbackJobAllocation(ctx, jobID, allocationID, reason, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) RollbackAssignment(ctx context.Context, jobID, assignmentID, reason string, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.RollbackAssignment(ctx, jobID, assignmentID, reason, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) RejectAssignment(ctx context.Context, jobID, assignmentID, reason string, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.RejectAssignment(ctx, jobID, assignmentID, reason, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) RequestJobCancellation(ctx context.Context, id, reason string, grace time.Duration, now time.Time) (domain.Job, bool, error) {
	return afterBoolCall(func() (domain.Job, bool, error) {
		return store.Store.RequestJobCancellation(ctx, id, reason, grace, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) RequestJobPreemption(ctx context.Context, preemptor, node string, victims []string, reason string, grace time.Duration, now time.Time) ([]domain.Job, error) {
	return afterCall(func() ([]domain.Job, error) {
		return store.Store.RequestJobPreemption(ctx, preemptor, node, victims, reason, grace, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) RejectJobCancellation(ctx context.Context, assignment, reason string, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.RejectJobCancellation(ctx, assignment, reason, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) ApplyAssignmentReport(ctx context.Context, report domain.AssignmentReport) (domain.Job, bool, error) {
	return afterBoolCall(func() (domain.Job, bool, error) {
		return store.Store.ApplyAssignmentReport(ctx, report)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) ApplyAssignmentLog(ctx context.Context, report domain.AssignmentLogSnapshot) (domain.Job, bool, error) {
	return afterBoolCall(func() (domain.Job, bool, error) {
		return store.Store.ApplyAssignmentLog(ctx, report)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) ReconcileOrphanedJobs(ctx context.Context, now time.Time) ([]domain.Job, error) {
	return afterCall(func() ([]domain.Job, error) {
		return store.Store.ReconcileOrphanedJobs(ctx, now)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) PutQueuePolicy(ctx context.Context, policy domain.QueuePolicy) error {
	if err := store.Store.PutQueuePolicy(ctx, policy); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) CreateDiscoveryScan(ctx context.Context, scan domain.DiscoveryScan) error {
	if err := store.Store.CreateDiscoveryScan(ctx, scan); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) FinishDiscoveryScan(ctx context.Context, scan domain.DiscoveryScan, candidates []domain.DiscoveryCandidate) error {
	if err := store.Store.FinishDiscoveryScan(ctx, scan, candidates); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) UpdateDiscoveryCandidate(ctx context.Context, id string, update func(*domain.DiscoveryCandidate) error, audit *domain.AuditEvent) (domain.DiscoveryCandidate, error) {
	return afterCall(func() (domain.DiscoveryCandidate, error) {
		return store.Store.UpdateDiscoveryCandidate(ctx, id, update, audit)
	}, func() error { return store.persist(ctx) })
}
func (store *Store) PutBuild(ctx context.Context, build domain.Build) error {
	if err := store.Store.PutBuild(ctx, build); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) DeleteBuild(ctx context.Context, id string) error {
	if err := store.Store.DeleteBuild(ctx, id); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) PutBuildRetentionPolicy(ctx context.Context, policy domain.BuildRetentionPolicy) error {
	if err := store.Store.PutBuildRetentionPolicy(ctx, policy); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) AppendAuditEvent(ctx context.Context, event domain.AuditEvent) error {
	if err := store.Store.AppendAuditEvent(ctx, event); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) CreateAPIToken(ctx context.Context, token domain.APIToken, digest [sha256.Size]byte, audit domain.AuditEvent) error {
	if err := store.Store.CreateAPIToken(ctx, token, digest, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) UpdateAPIToken(ctx context.Context, id string, update func(*domain.APIToken) error, audit *domain.AuditEvent) (domain.APIToken, error) {
	return afterCall(func() (domain.APIToken, error) {
		return store.Store.UpdateAPIToken(ctx, id, update, audit)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) CreateFirstUser(ctx context.Context, user domain.User, passwordHash string, audit domain.AuditEvent) error {
	if err := store.Store.CreateFirstUser(ctx, user, passwordHash, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}

func (store *Store) CreateUser(ctx context.Context, user domain.User, passwordHash string, audit domain.AuditEvent) (domain.User, error) {
	return afterCall(func() (domain.User, error) {
		return store.Store.CreateUser(ctx, user, passwordHash, audit)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) UpdateUser(ctx context.Context, id string, update func(*domain.User) error, audit domain.AuditEvent) (domain.User, error) {
	return afterCall(func() (domain.User, error) {
		return store.Store.UpdateUser(ctx, id, update, audit)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) CreateUserGroup(ctx context.Context, group domain.UserGroup, audit domain.AuditEvent) error {
	if err := store.Store.CreateUserGroup(ctx, group, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}

func (store *Store) UpdateUserGroup(ctx context.Context, id string, update func(*domain.UserGroup) error, audit domain.AuditEvent) (domain.UserGroup, error) {
	return afterCall(func() (domain.UserGroup, error) {
		return store.Store.UpdateUserGroup(ctx, id, update, audit)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) DeleteUserGroup(ctx context.Context, id string, audit domain.AuditEvent) ([]domain.User, error) {
	return afterCall(func() ([]domain.User, error) {
		return store.Store.DeleteUserGroup(ctx, id, audit)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) PutAccessSettings(ctx context.Context, settings domain.AccessSettings, audit domain.AuditEvent) error {
	if err := store.Store.PutAccessSettings(ctx, settings, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}

func (store *Store) ApplyAssignmentUsage(ctx context.Context, snapshot domain.AssignmentUsageSnapshot) (domain.Job, bool, error) {
	job, changed, err := store.Store.ApplyAssignmentUsage(ctx, snapshot)
	if err != nil || !changed {
		return job, changed, err
	}
	if err := store.persist(ctx); err != nil {
		return domain.Job{}, false, err
	}
	return job, true, nil
}

func (store *Store) PutBillingRate(ctx context.Context, rate domain.BillingRate, audit domain.AuditEvent) error {
	if err := store.Store.PutBillingRate(ctx, rate, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}

func (store *Store) UpdateGroupJobStorageQuota(ctx context.Context, groupID string, quotaBytes int64, now time.Time) ([]domain.Job, error) {
	return afterCall(func() ([]domain.Job, error) {
		return store.Store.UpdateGroupJobStorageQuota(ctx, groupID, quotaBytes, now)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) UpdateUserJobStorageQuota(ctx context.Context, ownerID string, quotaBytes int64, now time.Time) ([]domain.Job, error) {
	return afterCall(func() ([]domain.Job, error) {
		return store.Store.UpdateUserJobStorageQuota(ctx, ownerID, quotaBytes, now)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) SetGroupJobsSuspended(ctx context.Context, groupID string, suspended bool, reason string, now time.Time) ([]domain.Job, error) {
	return afterCall(func() ([]domain.Job, error) {
		return store.Store.SetGroupJobsSuspended(ctx, groupID, suspended, reason, now)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) FailQueuedJob(ctx context.Context, jobID, reason string, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.FailQueuedJob(ctx, jobID, reason, now)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) UpdateQueuedJobBlock(ctx context.Context, jobID string, block *domain.JobBlockReason, now time.Time) (domain.Job, error) {
	return afterCall(func() (domain.Job, error) {
		return store.Store.UpdateQueuedJobBlock(ctx, jobID, block, now)
	}, func() error { return store.persist(ctx) })
}

func (store *Store) Close() error {
	syncErr := store.sync(context.Background())
	localErr := store.Store.Close()
	var unlocked bool
	unlockErr := store.lockConn.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock($1)`, store.lockKey).Scan(&unlocked)
	connErr := store.lockConn.Close()
	dbErr := store.database.Close()
	return errors.Join(syncErr, localErr, unlockErr, connErr, dbErr)
}

func (store *Store) PutClusterBranding(ctx context.Context, expected uint64, value domain.ClusterBranding, audit domain.AuditEvent) error {
	if err := store.Store.PutClusterBranding(ctx, expected, value, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}

func (store *Store) PutAlertPolicy(ctx context.Context, expected uint64, value domain.AlertPolicy, audit domain.AuditEvent) error {
	if err := store.Store.PutAlertPolicy(ctx, expected, value, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}

func (store *Store) PutNodeHealth(ctx context.Context, snapshot domain.NodeHealthSnapshot, events []domain.SystemEvent) error {
	if err := store.Store.PutNodeHealth(ctx, snapshot, events); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) AppendSystemNote(ctx context.Context, event domain.SystemEvent, audit domain.AuditEvent) error {
	if err := store.Store.AppendSystemNote(ctx, event, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}
func (store *Store) PutBMCAction(ctx context.Context, action domain.BMCAction, audit domain.AuditEvent) error {
	if err := store.Store.PutBMCAction(ctx, action, audit); err != nil {
		return err
	}
	return store.persist(ctx)
}
