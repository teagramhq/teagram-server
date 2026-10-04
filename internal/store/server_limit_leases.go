package store

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const serverLimitLeaseLockClass = 0x74674c53 // "tgLS"

// LimitLease is one slot held in a cluster-wide concurrent server limit.
// Expired rows are reclaimed by the normal rate-limit sweeper.
type LimitLease struct {
	ID        [16]byte
	SubjectID int64
	Surface   string
}

// TryAcquireLimitLease atomically claims one of limit slots for subjectID and
// surface. A nil denial means the slot was acquired; a denial carries the time
// until the earliest current lease expires. Expired leases are removed before
// the count is checked, so crashed replicas cannot hold a slot forever.
//
// The transaction advisory lock serializes admissions for one subject/surface
// pair across every store connection. Every lease writer takes it before
// reading or changing rows, so acquisition and release use one lock order. It
// is released before this method returns, so a live lease never holds a
// database connection or lock.
func (s *Store) TryAcquireLimitLease(
	ctx context.Context,
	subjectID int64,
	surface string,
	limit int,
	ttl time.Duration,
) (*LimitLease, *RateLimitResult, error) {
	if limit <= 0 {
		return nil, nil, nil
	}
	if strings.TrimSpace(surface) == "" {
		return nil, nil, errors.New("acquire limit lease: surface is empty")
	}
	if ttl <= 0 {
		return nil, nil, fmt.Errorf("acquire limit lease %q: ttl must be positive", surface)
	}

	for range 3 {
		var id [16]byte
		if _, err := cryptorand.Read(id[:]); err != nil {
			return nil, nil, fmt.Errorf("acquire limit lease %q: generate id: %w", surface, err)
		}
		lease, denied, retry, err := s.tryAcquireLimitLeaseOnce(ctx, subjectID, surface, limit, ttl, id)
		if err != nil {
			return nil, nil, err
		}
		if retry {
			continue
		}
		return lease, denied, nil
	}
	return nil, nil, fmt.Errorf("acquire limit lease %q: admission did not settle after retries", surface)
}

func (s *Store) tryAcquireLimitLeaseOnce(
	ctx context.Context,
	subjectID int64,
	surface string,
	limit int,
	ttl time.Duration,
	id [16]byte,
) (*LimitLease, *RateLimitResult, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, fmt.Errorf("acquire limit lease %q: begin: %w", surface, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	lockKey := fmt.Sprintf("%d:%s", subjectID, surface)
	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1, hashtext($2))",
		serverLimitLeaseLockClass, lockKey,
	); err != nil {
		return nil, nil, false, fmt.Errorf("acquire limit lease %q: advisory lock: %w", surface, err)
	}

	qtx := s.q.WithTx(tx)
	if err := qtx.DeleteExpiredServerLimitLeasesForSubject(ctx, db.DeleteExpiredServerLimitLeasesForSubjectParams{
		SubjectID: subjectID,
		Surface:   surface,
	}); err != nil {
		return nil, nil, false, fmt.Errorf("acquire limit lease %q: prune expired: %w", surface, err)
	}
	current, err := qtx.CountActiveServerLimitLeases(ctx, db.CountActiveServerLimitLeasesParams{
		SubjectID: subjectID,
		Surface:   surface,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("acquire limit lease %q: count active: %w", surface, err)
	}
	if current >= int64(limit) {
		earliest, err := qtx.GetEarliestServerLimitLeaseExpiry(ctx, db.GetEarliestServerLimitLeaseExpiryParams{
			SubjectID: subjectID,
			Surface:   surface,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if err := tx.Commit(ctx); err != nil {
					return nil, nil, false, fmt.Errorf("acquire limit lease %q: commit stale denial: %w", surface, err)
				}
				return nil, nil, true, nil
			}
			return nil, nil, false, fmt.Errorf("acquire limit lease %q: get earliest expiry: %w", surface, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, fmt.Errorf("acquire limit lease %q: commit denial: %w", surface, err)
		}
		return nil, &RateLimitResult{Wait: waitUntil(s.now(), earliest.Time)}, false, nil
	}

	rows, err := qtx.InsertServerLimitLease(ctx, db.InsertServerLimitLeaseParams{
		LeaseID:   id[:],
		SubjectID: subjectID,
		Surface:   surface,
		Column4:   pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true},
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("acquire limit lease %q: insert: %w", surface, err)
	}
	if rows == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, fmt.Errorf("acquire limit lease %q: commit collision: %w", surface, err)
		}
		return nil, nil, true, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, fmt.Errorf("acquire limit lease %q: commit: %w", surface, err)
	}
	return &LimitLease{ID: id, SubjectID: subjectID, Surface: surface}, nil, false, nil
}

// ReleaseLimitLease returns a previously acquired slot. It is safe to call
// after expiry or more than once.
func (s *Store) ReleaseLimitLease(ctx context.Context, lease *LimitLease) error {
	if lease == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("release limit lease %q: begin: %w", lease.Surface, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	lockKey := fmt.Sprintf("%d:%s", lease.SubjectID, lease.Surface)
	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1, hashtext($2))",
		serverLimitLeaseLockClass, lockKey,
	); err != nil {
		return fmt.Errorf("release limit lease %q: advisory lock: %w", lease.Surface, err)
	}
	if _, err := s.q.WithTx(tx).DeleteServerLimitLease(ctx, db.DeleteServerLimitLeaseParams{
		LeaseID:   lease.ID[:],
		SubjectID: lease.SubjectID,
		Surface:   lease.Surface,
	}); err != nil {
		return fmt.Errorf("release limit lease %q: %w", lease.Surface, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("release limit lease %q: commit: %w", lease.Surface, err)
	}
	return nil
}
