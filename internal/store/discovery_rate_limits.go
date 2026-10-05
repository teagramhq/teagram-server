package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	discoveryRateLimitLockClass = 0x64697363 // "disc"
	discoveryGlobalSurface      = "local_discovery_global"
	discoveryNetworkSurface     = "local_discovery_network"
)

// CheckDiscoveryRateLimit checks the shared global and per-network discovery
// budgets in one transaction. A per-network denial rolls back the global token
// so the two configured bounds remain independent.
func (s *Store) CheckDiscoveryRateLimit(
	ctx context.Context,
	addr netip.Addr,
	global RateLimitConfig,
	perNetwork RateLimitConfig,
) (*RateLimitResult, error) {
	if !global.Enabled() && !perNetwork.Enabled() {
		return nil, nil //nolint:nilnil // disabled limits admit every discovery request
	}

	key, hasNetwork := IPBucketKey(addr)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("check discovery rate limit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1, hashtext($2))",
		discoveryRateLimitLockClass, "local_discovery",
	); err != nil {
		return nil, fmt.Errorf("check discovery rate limit: advisory lock: %w", err)
	}
	qtx := s.q.WithTx(tx)
	if global.Enabled() {
		denied, err := checkDiscoveryRateLimitSubject(ctx, qtx, 0, discoveryGlobalSurface, global, s.now)
		if err != nil {
			return nil, fmt.Errorf("check discovery global rate limit: %w", err)
		}
		if denied != nil {
			return denied, nil
		}
	}
	if perNetwork.Enabled() && hasNetwork {
		denied, err := checkDiscoveryRateLimitSubject(ctx, qtx, ipRateLimitSubjectID(key), discoveryNetworkSurface, perNetwork, s.now)
		if err != nil {
			return nil, fmt.Errorf("check discovery network rate limit: %w", err)
		}
		if denied != nil {
			return denied, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("check discovery rate limit: commit: %w", err)
	}
	return nil, nil //nolint:nilnil // both limits allow the request
}

func checkDiscoveryRateLimitSubject(
	ctx context.Context,
	q *db.Queries,
	subjectID int64,
	surface string,
	cfg RateLimitConfig,
	now func() time.Time,
) (*RateLimitResult, error) {
	_, err := q.TryConsumeRateLimit(ctx, db.TryConsumeRateLimitParams{
		SubjectID:  subjectID,
		Surface:    surface,
		Column3:    pgtype.Interval{Microseconds: cfg.Window.Microseconds(), Valid: true},
		TokenCount: int32(cfg.Limit), //nolint:gosec // config validation bounds active limits to positive ints
	})
	if err == nil {
		return nil, nil //nolint:nilnil // token consumed
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("consume %s: %w", surface, err)
	}
	expiresAt, err := q.GetRateLimitExpiresAt(ctx, db.GetRateLimitExpiresAtParams{
		SubjectID: subjectID,
		Surface:   surface,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil //nolint:nilnil // the rate-limit sweeper removed the expired row
		}
		return nil, fmt.Errorf("get %s expiry: %w", surface, err)
	}
	return &RateLimitResult{Wait: waitUntil(now(), expiresAt.Time)}, nil
}

func ipRateLimitSubjectID(key netip.Prefix) int64 {
	digest := sha256.Sum256([]byte(key.String()))
	// User ids are positive sequence values; keeping network subjects negative
	// prevents the same numeric key from aliasing an account budget.
	value := binary.BigEndian.Uint64(digest[:8]) & ((uint64(1) << 63) - 1)
	return -int64(value) - 1
}
