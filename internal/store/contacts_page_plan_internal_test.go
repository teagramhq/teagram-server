package store

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// The contacts page must never be cached as a generic plan. A cached statement
// on a pooled connection switches to a generic plan after five executions,
// and that plan ignores the owner's contact list.
func TestContactsWithUsersNeverCachesGenericPlan(t *testing.T) {
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
		 SELECT n, '+contact-plan' || n, 'Plan ' || n FROM generate_series(1, 50) n`,
		`INSERT INTO user_contacts (owner_id, contact_id)
		 SELECT 1, n FROM generate_series(2, 50) n`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed contacts plan: %v", err)
		}
	}
	// Force generic plans for every cached statement on this session. The page
	// query must not be cached at all, so no prepared statement carries its text.
	if _, err := pool.Exec(ctx, "SET plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatal(err)
	}

	s := &Store{pool: pool, q: db.New(pool)}
	for range 6 {
		rows, total, err := s.ContactsWithUsers(ctx, 1)
		if err != nil {
			t.Fatalf("list contacts with users: %v", err)
		}
		if len(rows) != 49 || total != 49 {
			t.Fatalf("contacts with users = %d rows, total %d; want 49", len(rows), total)
		}
	}

	var cached int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE statement LIKE '-- name: ContactsWithUsers%'`).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if cached != 0 {
		t.Fatalf("contacts page is cached in %d prepared statement(s), want none", cached)
	}

}
