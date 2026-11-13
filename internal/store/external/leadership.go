package external

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"time"
)

var ErrLeadershipLost = errors.New("controller leadership is unavailable")
var ErrPrimaryReady = errors.New("preferred primary is ready for handback")
var ErrControllerConfiguration = errors.New("invalid controller pair configuration")

func validateControllerConfig(c Config) error {
	if c.ControllerRole == "" {
		if c.ControllerNodeID != "" {
			return fmt.Errorf("%w: node identity requires a controller role", ErrControllerConfiguration)
		}
		return nil
	}
	if (c.ControllerRole != "primary" && c.ControllerRole != "backup") || c.ControllerNodeID == "" || len(c.ControllerNodeID) > 128 || len(c.TrustDigest) != 64 || (c.FailbackDelay != 0 && c.FailbackDelay < time.Second) {
		return fmt.Errorf("%w: primary/backup, node identity and shared trust digest are required", ErrControllerConfiguration)
	}
	return nil
}

func controllerKey(namespace, role string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("xirang-controller:" + namespace + ":" + role))
	return int64(h.Sum64())
}

// The persistent pair admits exactly one named primary and one named backup.
// Role locks limit each slot to one live process. The original state lock is
// still the sole authority for serving APIs, panel content and Agent streams.
func prepareController(ctx context.Context, conn *sql.Conn, c Config) (int64, int64, error) {
	if c.ControllerRole == "" {
		var exists bool
		if err := conn.QueryRowContext(ctx, `SELECT to_regclass('xirang_controller_members') IS NOT NULL`).Scan(&exists); err != nil {
			return 0, 0, err
		}
		if exists {
			var paired bool
			if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM xirang_controller_members WHERE namespace=$1)`, c.Namespace).Scan(&paired); err != nil {
				return 0, 0, err
			}
			if paired {
				return 0, 0, fmt.Errorf("%w: this namespace requires an assigned primary or backup", ErrControllerConfiguration)
			}
		}
		return 0, 0, nil
	}
	// Serialize schema/slot creation across simultaneous startups.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, controllerKey("schema", "v1")); err != nil {
		return 0, 0, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS xirang_controller_members (
 namespace TEXT NOT NULL, role TEXT NOT NULL CHECK (role IN ('primary','backup')),
 node_id TEXT NOT NULL, trust_digest TEXT NOT NULL,
 PRIMARY KEY(namespace,role), UNIQUE(namespace,node_id))`); err != nil {
		return 0, 0, err
	}
	var mismatch bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM xirang_controller_members WHERE namespace=$1 AND trust_digest<>$2)`, c.Namespace, c.TrustDigest).Scan(&mismatch); err != nil {
		return 0, 0, err
	}
	if mismatch {
		return 0, 0, fmt.Errorf("%w: paired controllers must share CA and administrator identity", ErrControllerConfiguration)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO xirang_controller_members(namespace,role,node_id,trust_digest) VALUES($1,$2,$3,$4)
 ON CONFLICT(namespace,role) DO UPDATE SET node_id=EXCLUDED.node_id WHERE xirang_controller_members.node_id=EXCLUDED.node_id`, c.Namespace, c.ControllerRole, c.ControllerNodeID, c.TrustDigest)
	if err != nil {
		return 0, 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if count != 1 {
		return 0, 0, fmt.Errorf("%w: controller slot belongs to another node", ErrControllerConfiguration)
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	key := controllerKey(c.Namespace, c.ControllerRole)
	var acquired bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		return 0, 0, err
	}
	if !acquired {
		return 0, 0, fmt.Errorf("%w: controller role is already running", ErrControllerConfiguration)
	}
	return key, controllerKey(c.Namespace, "primary"), nil
}

func rolePresent(ctx context.Context, conn *sql.Conn, key int64) (bool, error) {
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		return false, err
	}
	if !acquired {
		return true, nil
	}
	var unlocked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked); err != nil {
		return false, err
	}
	return false, nil
}

// CheckLeadership is also called before serving a new HTTP request. Once a
// persistence/lease check fails, this process cannot become healthy again using
// its stale cache; the lifecycle must close it and reload committed state.
func (s *Store) CheckLeadership(ctx context.Context) error {
	if s.fenced.Load() {
		return ErrLeadershipLost
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.fenced.Load() {
		return ErrLeadershipLost
	}
	var pid int64
	if err := s.lockConn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil || pid != s.leaderPID {
		s.fenced.Store(true)
		return ErrLeadershipLost
	}
	return nil
}

func (s *Store) WatchLeadership(ctx context.Context) <-chan error {
	changes := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := s.CheckLeadership(check)
			if err == nil && s.role == "backup" {
				s.syncMu.Lock()
				present, e := rolePresent(check, s.lockConn, s.primaryKey)
				if e != nil {
					err = ErrLeadershipLost
					s.fenced.Store(true)
				} else if !present {
					s.primarySince = time.Time{}
				} else if s.primarySince.IsZero() {
					s.primarySince = time.Now()
				} else if time.Since(s.primarySince) >= s.failbackDelay {
					err = ErrPrimaryReady
				}
				s.syncMu.Unlock()
			}
			cancel()
			if err != nil {
				changes <- err
				return
			}
		}
	}()
	return changes
}

func (s *Store) ControllerMembers(ctx context.Context) (map[string]string, error) {
	members := map[string]string{}
	if s.role == "" {
		return members, nil
	}
	rows, err := s.database.QueryContext(ctx, `SELECT node_id,role FROM xirang_controller_members WHERE namespace=$1`, s.namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		members[id] = role
	}
	return members, rows.Err()
}
