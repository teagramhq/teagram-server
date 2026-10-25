package store

import (
	"time"

	"github.com/jackc/pgx/v5"
)

// PoolConfig bounds pooled database sessions independently of CPU detection.
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnIdleTime time.Duration
}

// WithPoolConfig overrides pool limits in the DSN. Without this option Open
// retains pgx's DSN settings for callers that manage their own pool budget.
func WithPoolConfig(cfg PoolConfig) Option {
	return func(s *Store) { s.poolConfig = &cfg }
}

// WithQueryTracer installs tracer on every pooled connection. The performance
// harness uses it to count each RPC's queries; the server path leaves it unset.
func WithQueryTracer(tracer pgx.QueryTracer) Option {
	return func(s *Store) { s.queryTracer = tracer }
}
