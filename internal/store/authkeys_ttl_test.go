package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type authorizationTTLSettings interface {
	AuthorizationTTLDays(context.Context, int64) (int, error)
	SetAuthorizationTTL(context.Context, int64, int64, int) error
}

type authorizationTTLSweeper interface {
	SweepExpiredAuthorizations(context.Context, int) ([]store.AuthKey, error)
}

func TestAuthorizationTTLSweepUsesOrderedBoundKeyIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner, err := s.CreateUser(ctx, "+15551260799")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to inspect authorization sweep plan: %v", err)
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
		  AND indexname = 'auth_keys_bound_last_seen_id_idx'
	`).Scan(&indexDefinition); err != nil {
		t.Fatalf("read authorization sweep index: %v", err)
	}
	if !strings.Contains(indexDefinition, "(last_seen_at, id)") || !strings.Contains(indexDefinition, "WHERE (user_id IS NOT NULL)") {
		t.Fatalf("authorization sweep index definition = %q, want ordered partial index for bound keys", indexDefinition)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO auth_keys (id, key_value, user_id, last_seen_at)
		SELECT 6000000000000000000 + n, decode('00', 'hex'), $1,
		       now() - interval '400 days' - n * interval '1 second'
		FROM generate_series(1, 10000) AS n
	`, owner.ID); err != nil {
		t.Fatalf("seed expired authorizations: %v", err)
	}
	if _, err := conn.Exec(ctx, `ANALYZE auth_keys`); err != nil {
		t.Fatalf("analyze auth keys: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatalf("prefer indexed authorization sweep plan: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET enable_hashjoin = off`); err != nil {
		t.Fatalf("prefer ordered authorization sweep join: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET enable_mergejoin = off`); err != nil {
		t.Fatalf("prefer ordered authorization sweep join: %v", err)
	}
	rows, err := conn.Query(ctx, `EXPLAIN (COSTS OFF)
		SELECT ak.id, ak.user_id
		FROM auth_keys ak
		JOIN users u ON u.id = ak.user_id
		WHERE ak.user_id IS NOT NULL
		  AND ak.last_seen_at < now() - (u.authorization_ttl_days * interval '1 day')
		  AND NOT (
		      u.login_mode = 'username'
		      AND NOT EXISTS (SELECT 1 FROM user_passwords up WHERE up.user_id = u.id)
		  )
		ORDER BY ak.last_seen_at, ak.id
		LIMIT 1000
		FOR UPDATE OF ak SKIP LOCKED
	`)
	if err != nil {
		t.Fatalf("explain authorization sweep selection: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan authorization sweep plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read authorization sweep plan: %v", err)
	}
	planText := plan.String()
	if !strings.Contains(planText, "auth_keys_bound_last_seen_id_idx") || strings.Contains(planText, "Sort") {
		t.Fatalf("authorization sweep plan does not use an ordered bound-key index:\n%s", planText)
	}
}

func requireAuthorizationTTLSettings(t *testing.T, s *store.Store) authorizationTTLSettings {
	t.Helper()
	settings, ok := any(s).(authorizationTTLSettings)
	if !ok {
		t.Fatal("Store does not expose authorization TTL settings")
	}
	return settings
}

func TestAuthorizationTTLSettingsAreOwnerScopedAndRefreshCaller(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15551260801")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	foreign, err := s.CreateUser(ctx, "+15551260802")
	if err != nil {
		t.Fatalf("create foreign owner: %v", err)
	}
	const callerKey = int64(0x6801)
	const foreignKey = int64(0x6802)
	saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, foreignKey, foreign.ID)

	settings := requireAuthorizationTTLSettings(t, s)
	if got, err := settings.AuthorizationTTLDays(ctx, owner.ID); err != nil || got != 365 {
		t.Fatalf("new owner's authorization TTL = %d, err=%v, want 365", got, err)
	}
	if got, err := settings.AuthorizationTTLDays(ctx, foreign.ID); err != nil || got != 365 {
		t.Fatalf("foreign owner's authorization TTL = %d, err=%v, want 365", got, err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to backdate caller: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '8 days' WHERE id = $1`, callerKey); err != nil {
		t.Fatalf("backdate caller: %v", err)
	}
	if err := settings.SetAuthorizationTTL(ctx, owner.ID, callerKey, 7); err != nil {
		t.Fatalf("set owner's authorization TTL: %v", err)
	}
	if got, err := settings.AuthorizationTTLDays(ctx, owner.ID); err != nil || got != 7 {
		t.Fatalf("owner's authorization TTL = %d, err=%v, want 7", got, err)
	}
	if got, err := settings.AuthorizationTTLDays(ctx, foreign.ID); err != nil || got != 365 {
		t.Fatalf("foreign owner's authorization TTL changed to %d, err=%v", got, err)
	}
	if err := settings.SetAuthorizationTTL(ctx, owner.ID, callerKey, 1); err == nil {
		t.Fatal("store accepted an unsupported authorization TTL")
	}
	if err := settings.SetAuthorizationTTL(ctx, owner.ID, callerKey, int(^uint(0)>>1)); err == nil {
		t.Fatal("store accepted an authorization TTL too large to represent in the database")
	}
	if _, err := conn.Exec(ctx, `UPDATE users SET authorization_ttl_days = 1 WHERE id = $1`, owner.ID); err == nil {
		t.Fatal("database accepted an unsupported authorization TTL")
	}
	if got, err := settings.AuthorizationTTLDays(ctx, owner.ID); err != nil || got != 7 {
		t.Fatalf("rejected database TTL write changed value to %d, err=%v; want 7", got, err)
	}
	key, ok, err := s.AuthKeyByID(ctx, callerKey)
	if err != nil || !ok {
		t.Fatalf("read caller key: ok=%v err=%v", ok, err)
	}
	if time.Since(key.LastSeenAt) > time.Minute {
		t.Fatalf("caller was not refreshed by TTL write: last_seen_at=%s", key.LastSeenAt)
	}

	if err := settings.SetAuthorizationTTL(ctx, owner.ID, foreignKey, 30); err == nil {
		t.Fatal("setting TTL with a foreign caller key succeeded")
	}
	if got, err := settings.AuthorizationTTLDays(ctx, owner.ID); err != nil || got != 7 {
		t.Fatalf("foreign caller changed owner's TTL to %d, err=%v; want 7", got, err)
	}
}

func TestAuthorizationTTLSweepRevokesOnlyIdleBoundKeys(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15551260811")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	foreign, err := s.CreateUser(ctx, "+15551260812")
	if err != nil {
		t.Fatalf("create foreign owner: %v", err)
	}
	provisional, err := s.CreateUsernameUser(ctx, "ttlprovisional", "TTL", "Provisional")
	if err != nil {
		t.Fatalf("create provisional user: %v", err)
	}

	const (
		callerKey      = int64(0x6811)
		idleKey        = int64(0x6812)
		recentKey      = int64(0x6813)
		foreignKey     = int64(0x6814)
		pendingKey     = int64(0x6815)
		provisionalKey = int64(0x6816)
		idleKey2       = int64(0x6817)
		idleKey3       = int64(0x6818)
	)
	saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, idleKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, recentKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, idleKey2, owner.ID)
	saveBoundAuthKey(t, ctx, s, idleKey3, owner.ID)
	saveBoundAuthKey(t, ctx, s, foreignKey, foreign.ID)
	if err := s.SaveAuthKey(ctx, pendingKey, []byte("pending")); err != nil {
		t.Fatalf("save pending key: %v", err)
	}
	if err := s.SetPendingUser(ctx, pendingKey, owner.ID); err != nil {
		t.Fatalf("set pending owner: %v", err)
	}
	saveBoundAuthKey(t, ctx, s, provisionalKey, provisional.ID)

	settings := requireAuthorizationTTLSettings(t, s)
	if err := settings.SetAuthorizationTTL(ctx, owner.ID, callerKey, 7); err != nil {
		t.Fatalf("set owner TTL: %v", err)
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
	for _, keyID := range []int64{idleKey, idleKey2, idleKey3, foreignKey, pendingKey, provisionalKey} {
		if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '8 days' WHERE id = $1`, keyID); err != nil {
			t.Fatalf("age auth key %d: %v", keyID, err)
		}
	}
	if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '6 days 23 hours' WHERE id = $1`, recentKey); err != nil {
		t.Fatalf("age recent auth key: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE users SET authorization_ttl_days = 7 WHERE id = $1`, provisional.ID); err != nil {
		t.Fatalf("set provisional TTL fixture: %v", err)
	}

	sweeper, ok := any(s).(authorizationTTLSweeper)
	if !ok {
		t.Fatal("Store does not expose authorization TTL sweep")
	}
	removed, err := sweeper.SweepExpiredAuthorizations(ctx, 1)
	if err != nil {
		t.Fatalf("sweep expired authorizations: %v", err)
	}
	removedIDs := make(map[int64]bool, len(removed))
	for _, key := range removed {
		if key.UserID != owner.ID {
			t.Fatalf("revoked key %d owner = %d, want %d", key.ID, key.UserID, owner.ID)
		}
		removedIDs[key.ID] = true
	}
	if len(removed) != 3 || len(removedIDs) != 3 || !removedIDs[idleKey] || !removedIDs[idleKey2] || !removedIDs[idleKey3] {
		t.Fatalf("revoked key ids = %v, want %d, %d, and %d across bounded batches", removedIDs, idleKey, idleKey2, idleKey3)
	}
	requireAuthKey(t, ctx, s, callerKey, true, owner.ID, 0)
	requireAuthKey(t, ctx, s, idleKey, false, 0, 0)
	requireAuthKey(t, ctx, s, recentKey, true, owner.ID, 0)
	requireAuthKey(t, ctx, s, idleKey2, false, 0, 0)
	requireAuthKey(t, ctx, s, idleKey3, false, 0, 0)
	requireAuthKey(t, ctx, s, foreignKey, true, foreign.ID, 0)
	requireAuthKey(t, ctx, s, pendingKey, true, 0, owner.ID)
	requireAuthKey(t, ctx, s, provisionalKey, true, provisional.ID, 0)
	var accountCount int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, owner.ID).Scan(&accountCount); err != nil {
		t.Fatalf("read owner after sweep: %v", err)
	}
	if accountCount != 1 {
		t.Fatalf("owner account count after session sweep = %d, want 1", accountCount)
	}
}

func TestSweepExpiredAuthorizationsRemovesExpiredKey(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15551260821")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const keyID = int64(0x6821)
	saveBoundAuthKey(t, ctx, s, keyID, owner.ID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to age auth key: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE users SET authorization_ttl_days = 7 WHERE id = $1`, owner.ID); err != nil {
		t.Fatalf("set owner TTL fixture: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '8 days' WHERE id = $1`, keyID); err != nil {
		t.Fatalf("age auth key: %v", err)
	}
	sweeper, ok := any(s).(authorizationTTLSweeper)
	if !ok {
		t.Fatal("Store does not expose authorization TTL sweep")
	}
	removed, err := sweeper.SweepExpiredAuthorizations(ctx, 1)
	if err != nil {
		t.Fatalf("sweep expired authorizations: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != keyID || removed[0].UserID != owner.ID {
		t.Fatalf("revoked keys = %+v, want key %d for owner %d", removed, keyID, owner.ID)
	}
	requireAuthKey(t, ctx, s, keyID, false, 0, 0)
}

func TestConcurrentAuthorizationSweepsDeleteEachKeyOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := pgtest.DSN(t)
	sA := openStore(t, dsn)
	sB := openStore(t, dsn)
	owner, err := sA.CreateUser(ctx, "+15551260891")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const keyCount = 32
	keyIDs := make([]int64, keyCount)
	for i := range keyIDs {
		keyIDs[i] = int64(0x6890 + i)
		saveBoundAuthKey(t, ctx, sA, keyIDs[i], owner.ID)
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
	if _, err := conn.Exec(ctx, `UPDATE users SET authorization_ttl_days = 7 WHERE id = $1`, owner.ID); err != nil {
		t.Fatalf("set owner TTL fixture: %v", err)
	}
	for _, keyID := range keyIDs {
		if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '8 days' WHERE id = $1`, keyID); err != nil {
			t.Fatalf("age auth key %d: %v", keyID, err)
		}
	}

	type sweepResult struct {
		keys []store.AuthKey
		err  error
	}
	start := make(chan struct{})
	results := make(chan sweepResult, 2)
	for _, replica := range []*store.Store{sA, sB} {
		go func(replica *store.Store) {
			<-start
			keys, err := replica.SweepExpiredAuthorizations(ctx, 3)
			results <- sweepResult{keys: keys, err: err}
		}(replica)
	}
	close(start)
	seen := make(map[int64]bool, keyCount)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent sweep: %v", result.err)
		}
		for _, key := range result.keys {
			if key.UserID != owner.ID {
				t.Fatalf("revoked key %d owner = %d, want %d", key.ID, key.UserID, owner.ID)
			}
			if seen[key.ID] {
				t.Fatalf("auth key %d was returned by both sweeps", key.ID)
			}
			seen[key.ID] = true
		}
	}
	if len(seen) != keyCount {
		t.Fatalf("concurrent sweeps revoked %d distinct keys, want %d", len(seen), keyCount)
	}
	for _, keyID := range keyIDs {
		requireAuthKey(t, ctx, sA, keyID, false, 0, 0)
	}
}

func TestAuthorizationTouchWinsOverConcurrentSweep(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner, err := s.CreateUser(ctx, "+15551260892")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const keyID = int64(0x6892)
	saveBoundAuthKey(t, ctx, s, keyID, owner.ID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to age auth key: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE users SET authorization_ttl_days = 7 WHERE id = $1`, owner.ID); err != nil {
		t.Fatalf("set owner TTL fixture: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() - interval '8 days' WHERE id = $1`, keyID); err != nil {
		t.Fatalf("age auth key: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin touch transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, `UPDATE auth_keys SET last_seen_at = now() WHERE id = $1`, keyID); err != nil {
		t.Fatalf("touch auth key: %v", err)
	}

	type sweepResult struct {
		keys []store.AuthKey
		err  error
	}
	resultCh := make(chan sweepResult, 1)
	go func() {
		keys, err := s.SweepExpiredAuthorizations(ctx, 10)
		resultCh <- sweepResult{keys: keys, err: err}
	}()
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("sweep while key is being touched: %v", result.err)
		}
		if len(result.keys) != 0 {
			t.Fatalf("sweep revoked locked, concurrently touched key: %+v", result.keys)
		}
	case <-time.After(time.Second):
		t.Fatal("sweep blocked behind a concurrent touch instead of skipping the locked key")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit touch: %v", err)
	}
	requireAuthKey(t, ctx, s, keyID, true, owner.ID, 0)
}
