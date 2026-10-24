package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type neverBoundAuthKeySweeper interface {
	SweepNeverBoundAuthKeys(context.Context, time.Duration, time.Duration, time.Duration, int) (int, error)
}

func requireNeverBoundAuthKeySweeper(t *testing.T, s *store.Store) neverBoundAuthKeySweeper {
	t.Helper()
	sweeper, ok := any(s).(neverBoundAuthKeySweeper)
	if !ok {
		t.Fatal("Store does not expose never-bound auth-key sweep")
	}
	return sweeper
}

func saveNeverBoundSweepKey(t *testing.T, ctx context.Context, s *store.Store, id int64) {
	t.Helper()
	if err := s.SaveAuthKey(ctx, id, []byte("never-bound sweep fixture")); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
}

func TestSweepNeverBoundAuthKeysUsesIdleAndMaximumAgeWithoutDeletingBoundOrPendingKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	sweeper := requireNeverBoundAuthKeySweeper(t, s)
	owner, err := s.CreateUser(ctx, "+15551260931")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	const (
		idleKey         = int64(0x739101)
		livePendingKey  = int64(0x739102)
		expiredPending  = int64(0x739103)
		everBoundKey    = int64(0x739104)
		maxAgeKey       = int64(0x739105)
		activeRecentKey = int64(0x739106)
	)
	for _, id := range []int64{idleKey, livePendingKey, expiredPending, everBoundKey, maxAgeKey, activeRecentKey} {
		saveNeverBoundSweepKey(t, ctx, s, id)
	}
	if _, _, err := s.StagePendingUser(ctx, livePendingKey, owner.ID, time.Hour); err != nil {
		t.Fatalf("stage live pending login: %v", err)
	}
	if _, _, err := s.StagePendingUser(ctx, expiredPending, owner.ID, time.Hour); err != nil {
		t.Fatalf("stage expired pending login: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, everBoundKey, owner.ID); err != nil {
		t.Fatalf("bind retained key: %v", err)
	}
	if _, _, err := s.LogOutAuthKey(ctx, everBoundKey); err != nil {
		t.Fatalf("log out retained key: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to age auth keys: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	for _, fixture := range []struct {
		id         int64
		createdAgo string
		seenAgo    string
		pendingAgo string
	}{
		{id: idleKey, createdAgo: "26 hours", seenAgo: "25 hours"},
		{id: livePendingKey, createdAgo: "26 hours", seenAgo: "25 hours"},
		{id: expiredPending, createdAgo: "26 hours", seenAgo: "25 hours", pendingAgo: "2 hours"},
		{id: everBoundKey, createdAgo: "30 days", seenAgo: "30 days"},
		{id: maxAgeKey, createdAgo: "73 hours", seenAgo: "1 hour"},
		{id: activeRecentKey, createdAgo: "1 hour", seenAgo: "1 minute"},
	} {
		query := `UPDATE auth_keys SET created_at = clock_timestamp() - interval '` + fixture.createdAgo + `', last_seen_at = clock_timestamp() - interval '` + fixture.seenAgo + `'`
		if fixture.pendingAgo != "" {
			query += `, pending_started_at = clock_timestamp() - interval '` + fixture.pendingAgo + `'`
		}
		query += ` WHERE id = $1`
		if _, err := conn.Exec(ctx, query, fixture.id); err != nil {
			t.Fatalf("age auth key fixture: %v", err)
		}
	}

	deleted, err := sweeper.SweepNeverBoundAuthKeys(ctx, 24*time.Hour, 72*time.Hour, time.Hour, 2)
	if err != nil {
		t.Fatalf("sweep never-bound auth keys: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("deleted count = %d, want 3", deleted)
	}
	for _, fixture := range []struct {
		id     int64
		exists bool
		name   string
	}{
		{id: idleKey, name: "idle never-bound", exists: false},
		{id: expiredPending, name: "expired pending login", exists: false},
		{id: maxAgeKey, name: "over maximum age", exists: false},
		{id: livePendingKey, name: "live pending login", exists: true},
		{id: everBoundKey, name: "previously bound", exists: true},
		{id: activeRecentKey, name: "recently active", exists: true},
	} {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM auth_keys WHERE id = $1)`, fixture.id).Scan(&exists); err != nil {
			t.Fatalf("check %s fixture: %v", fixture.name, err)
		}
		if exists != fixture.exists {
			t.Errorf("%s key exists = %v, want %v", fixture.name, exists, fixture.exists)
		}
	}
}

func TestSweepNeverBoundAuthKeysSkipsLockedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	sweeper := requireNeverBoundAuthKeySweeper(t, s)
	const keyID = int64(0x739201)
	saveNeverBoundSweepKey(t, ctx, s, keyID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to lock auth key: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin row lock: %v", err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback row lock: %v", err)
		}
	}()
	if _, err := tx.Exec(ctx, `UPDATE auth_keys SET last_seen_at = clock_timestamp() - interval '25 hours' WHERE id = $1`, keyID); err != nil {
		t.Fatalf("lock candidate key: %v", err)
	}
	deleted, err := sweeper.SweepNeverBoundAuthKeys(ctx, 24*time.Hour, 72*time.Hour, time.Hour, 1)
	if err != nil {
		t.Fatalf("sweep while candidate is locked: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted locked rows = %d, want 0", deleted)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release row lock: %v", err)
	}
	deleted, err = sweeper.SweepNeverBoundAuthKeys(ctx, 24*time.Hour, 72*time.Hour, time.Hour, 1)
	if err != nil {
		t.Fatalf("sweep after releasing candidate: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted rows after lock release = %d, want 1", deleted)
	}
}

func TestSweepNeverBoundAuthKeysLeavesConcurrentTouchBindAndPendingStartAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	sweeper := requireNeverBoundAuthKeySweeper(t, s)
	owner, err := s.CreateUser(ctx, "+15551260933")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const (
		touchedKey = int64(0x739301)
		boundKey   = int64(0x739302)
		pendingKey = int64(0x739303)
	)
	for _, id := range []int64{touchedKey, boundKey, pendingKey} {
		saveNeverBoundSweepKey(t, ctx, s, id)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to update auth keys concurrently: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE auth_keys SET created_at = clock_timestamp() - interval '26 hours', last_seen_at = clock_timestamp() - interval '25 hours' WHERE id = ANY($1::bigint[])`, []int64{touchedKey, boundKey, pendingKey}); err != nil {
		t.Fatalf("age concurrent auth-key fixtures: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent state change: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback concurrent state change: %v", err)
		}
	})
	rows, err := tx.Query(ctx, `SELECT id FROM auth_keys WHERE id = ANY($1::bigint[]) FOR UPDATE`, []int64{touchedKey, boundKey, pendingKey})
	if err != nil {
		t.Fatalf("lock auth-key fixtures: %v", err)
	}
	locked := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan locked auth key: %v", err)
		}
		locked++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read locked auth keys: %v", err)
	}
	rows.Close()
	if locked != 3 {
		t.Fatalf("locked rows = %d, want 3", locked)
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_keys SET last_seen_at = clock_timestamp() WHERE id = $1`, touchedKey); err != nil {
		t.Fatalf("touch locked key: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_keys SET user_id = $2 WHERE id = $1`, boundKey, owner.ID); err != nil {
		t.Fatalf("bind locked key: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_keys SET pending_user_id = $2, pending_started_at = clock_timestamp() WHERE id = $1`, pendingKey, owner.ID); err != nil {
		t.Fatalf("start pending login on locked key: %v", err)
	}

	deleted, err := sweeper.SweepNeverBoundAuthKeys(ctx, 24*time.Hour, 72*time.Hour, time.Hour, 3)
	if err != nil {
		t.Fatalf("sweep while concurrent auth-key changes are locked: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted concurrent candidates = %d, want 0", deleted)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit concurrent state changes: %v", err)
	}
	deleted, err = sweeper.SweepNeverBoundAuthKeys(ctx, 24*time.Hour, 72*time.Hour, time.Hour, 3)
	if err != nil {
		t.Fatalf("sweep after concurrent auth-key changes: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted updated auth keys = %d, want 0", deleted)
	}
}

func TestSweepNeverBoundAuthKeysRunsAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	sA := openStore(t, dsn)
	sB := openStore(t, dsn)
	sweeperA := requireNeverBoundAuthKeySweeper(t, sA)
	sweeperB := requireNeverBoundAuthKeySweeper(t, sB)
	const keyCount = 12
	ids := make([]int64, keyCount)
	for i := range ids {
		ids[i] = int64(0x739400 + i)
		saveNeverBoundSweepKey(t, ctx, sA, ids[i])
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to age replica fixtures: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE auth_keys SET created_at = clock_timestamp() - interval '26 hours', last_seen_at = clock_timestamp() - interval '25 hours' WHERE id = ANY($1::bigint[])`, ids); err != nil {
		t.Fatalf("age replica auth keys: %v", err)
	}
	type result struct {
		deleted int
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, sweeper := range []neverBoundAuthKeySweeper{sweeperA, sweeperB} {
		wg.Add(1)
		go func(sweeper neverBoundAuthKeySweeper) {
			defer wg.Done()
			<-start
			deleted, err := sweeper.SweepNeverBoundAuthKeys(ctx, 24*time.Hour, 72*time.Hour, time.Hour, 3)
			results <- result{deleted: deleted, err: err}
		}(sweeper)
	}
	close(start)
	wg.Wait()
	close(results)
	totalDeleted := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("replica sweep: %v", result.err)
		}
		totalDeleted += result.deleted
	}
	if totalDeleted != keyCount {
		t.Fatalf("replicas deleted %d keys, want %d", totalDeleted, keyCount)
	}
	var remaining int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM auth_keys WHERE id = ANY($1::bigint[])`, ids).Scan(&remaining); err != nil {
		t.Fatalf("count remaining replica keys: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining replica keys = %d, want 0", remaining)
	}
}

func TestNeverBoundAuthKeySweepUsesPartialIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	_ = openStore(t, dsn)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to inspect never-bound sweep plan: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	var indexDefinition string
	if err := conn.QueryRow(ctx, `
		SELECT indexdef
		FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND tablename = 'auth_keys'
		  AND indexname = 'auth_keys_never_bound_sweep_idx'
	`).Scan(&indexDefinition); err != nil {
		t.Fatalf("read never-bound sweep index: %v", err)
	}
	if !strings.Contains(indexDefinition, "(last_seen_at, id)") || !strings.Contains(indexDefinition, "user_id IS NULL") || !strings.Contains(indexDefinition, "ever_bound_at IS NULL") {
		t.Fatalf("never-bound sweep index definition = %q, want partial index on last_seen_at and id", indexDefinition)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO auth_keys (id, key_value, created_at, last_seen_at)
		SELECT 6100000000000000000 + n, decode('00', 'hex'), now() - interval '25 hours', now() - interval '25 hours'
		FROM generate_series(1, 10000) AS n
	`); err != nil {
		t.Fatalf("seed never-bound auth keys: %v", err)
	}
	if _, err := conn.Exec(ctx, `ANALYZE auth_keys`); err != nil {
		t.Fatalf("analyze auth keys: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("prefer indexed never-bound sweep plan: %v", err)
	}
	rows, err := conn.Query(ctx, `EXPLAIN (COSTS OFF)
		SELECT ak.id
		FROM auth_keys AS ak
		WHERE ak.user_id IS NULL
		  AND ak.ever_bound_at IS NULL
		  AND (ak.last_seen_at < clock_timestamp() - interval '24 hours'
		       OR ak.created_at < clock_timestamp() - interval '72 hours')
		  AND NOT (
		      ak.pending_user_id IS NOT NULL
		      AND ak.pending_started_at IS NOT NULL
		      AND ak.pending_started_at + interval '1 hour' > clock_timestamp()
		  )
		ORDER BY ak.last_seen_at, ak.id
		LIMIT 1000
		FOR UPDATE OF ak SKIP LOCKED
	`)
	if err != nil {
		t.Fatalf("explain never-bound sweep selection: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan never-bound sweep plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read never-bound sweep plan: %v", err)
	}
	if !strings.Contains(plan.String(), "auth_keys_never_bound_sweep_idx") || strings.Contains(plan.String(), "Sort") {
		t.Fatalf("never-bound sweep plan does not use the ordered partial index:\n%s", plan.String())
	}
}
