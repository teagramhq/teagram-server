package store

import "time"

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
