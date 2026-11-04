package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// A visibility batch over many owners must not be cached as a generic plan: a
// generic plan cannot size the owner array and runs far slower than a custom one.
func TestEvaluateLastSeenPrivacyLargeBatchNeverCachesGenericPlan(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`,
		`INSERT INTO users (id, phone, first_name)
		 SELECT n, '+privacy-plan' || n, 'Plan ' || n FROM generate_series(1, 1100) n`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed privacy plan: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, "SET plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatal(err)
	}

	owners := make([]User, 0, 1100)
	for id := int64(2); id <= 1100; id++ {
		owners = append(owners, User{ID: id})
	}
	s := &Store{pool: pool, q: db.New(pool), now: time.Now}
	for range 6 {
		visibility, err := s.EvaluateLastSeenPrivacyForViewer(ctx, 1, owners)
		if err != nil {
			t.Fatalf("evaluate last-seen privacy: %v", err)
		}
		if len(visibility) != len(owners) {
			t.Fatalf("visibility for %d owners, want %d", len(visibility), len(owners))
		}
	}

	var cached int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE statement LIKE '-- name: EvaluateLastSeenPrivacyForViewer%'`).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if cached != 0 {
		t.Fatalf("large visibility batch is cached in %d prepared statement(s), want none", cached)
	}
}
