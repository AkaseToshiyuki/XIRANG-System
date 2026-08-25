package bolt

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/xirang-grid/xirang/internal/discovery"
	"github.com/xirang-grid/xirang/internal/domain"
	"go.etcd.io/bbolt"
)

func (store *Store) CreateDiscoveryScan(ctx context.Context, scan domain.DiscoveryScan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(scan)
	if err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(discoveryScansBucket)
		if bucket.Get([]byte(scan.ID)) != nil {
			return discovery.ErrConflict
		}
		return bucket.Put([]byte(scan.ID), encoded)
	})
}

func (store *Store) FinishDiscoveryScan(ctx context.Context, scan domain.DiscoveryScan, candidates []domain.DiscoveryCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.database.Update(func(tx *bbolt.Tx) error {
		scans := tx.Bucket(discoveryScansBucket)
		if scans.Get([]byte(scan.ID)) == nil {
			return discovery.ErrNotFound
		}
		encodedScan, err := json.Marshal(scan)
		if err != nil {
			return err
		}
		if err := scans.Put([]byte(scan.ID), encodedScan); err != nil {
			return err
		}
		bucket := tx.Bucket(discoveryCandidatesBucket)
		for _, incoming := range candidates {
			if encoded := bucket.Get([]byte(incoming.ID)); encoded != nil {
				var current domain.DiscoveryCandidate
				if err := json.Unmarshal(encoded, &current); err != nil {
					return err
				}
				incoming.FirstSeenAt = current.FirstSeenAt
				incoming.Status, incoming.ApprovedAt, incoming.RejectedAt = current.Status, current.ApprovedAt, current.RejectedAt
				incoming.BootstrapTTL = current.BootstrapTTL
			}
			encoded, err := json.Marshal(incoming)
			if err != nil {
				return err
			}
			if err := bucket.Put([]byte(incoming.ID), encoded); err != nil {
				return err
			}
		}
		return putAudit(tx, domain.AuditEvent{
			ID: "discovery_scan_" + scan.ID, ActorType: "admin", ActorID: "bootstrap-admin",
			Action: "discovery.scan.completed", TargetKind: "discovery_scan", TargetID: scan.ID,
			Outcome: string(scan.Status), OccurredAt: *scan.FinishedAt,
		})
	})
}

func (store *Store) ListDiscoveryScans(ctx context.Context, limit int) ([]domain.DiscoveryScan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scans := make([]domain.DiscoveryScan, 0)
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(discoveryScansBucket).ForEach(func(_, encoded []byte) error {
			var scan domain.DiscoveryScan
			if err := json.Unmarshal(encoded, &scan); err != nil {
				return err
			}
			scans = append(scans, scan)
			return nil
		})
	})
	sort.Slice(scans, func(i, j int) bool { return scans[i].StartedAt.After(scans[j].StartedAt) })
	if len(scans) > limit {
		scans = scans[:limit]
	}
	return scans, err
}

func (store *Store) ListDiscoveryCandidates(ctx context.Context) ([]domain.DiscoveryCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	candidates := make([]domain.DiscoveryCandidate, 0)
	err := store.database.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(discoveryCandidatesBucket).ForEach(func(_, encoded []byte) error {
			var candidate domain.DiscoveryCandidate
			if err := json.Unmarshal(encoded, &candidate); err != nil {
				return err
			}
			candidates = append(candidates, candidate)
			return nil
		})
	})
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Address < candidates[j].Address })
	return candidates, err
}

func (store *Store) UpdateDiscoveryCandidate(
	ctx context.Context,
	id string,
	update func(*domain.DiscoveryCandidate) error,
	audit *domain.AuditEvent,
) (domain.DiscoveryCandidate, error) {
	if err := ctx.Err(); err != nil {
		return domain.DiscoveryCandidate{}, err
	}
	var updated domain.DiscoveryCandidate
	err := store.database.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(discoveryCandidatesBucket)
		encoded := bucket.Get([]byte(id))
		if encoded == nil {
			return discovery.ErrNotFound
		}
		var candidate domain.DiscoveryCandidate
		if err := json.Unmarshal(encoded, &candidate); err != nil {
			return err
		}
		if err := update(&candidate); err != nil {
			return err
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(id), encoded); err != nil {
			return err
		}
		if audit != nil {
			if err := putAudit(tx, *audit); err != nil {
				return err
			}
		}
		updated = candidate
		return nil
	})
	return updated, err
}
