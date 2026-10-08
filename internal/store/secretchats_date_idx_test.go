package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// dateIdxMigrationFile is the forward migration that adds the two
// party-and-date indexes this file pins.
const dateIdxMigrationFile = "20261008000067_secret_chat_party_date_idx.sql"

// wantReplayPredicate is the WHERE clause the lifecycle replay query must keep.
// The statement is fixed, not merely preferred: getDifference's replay is
// deliberately uncapped and returns every row past the client's date, so an
// index ticket may not rewrite the query its indexes serve. A LIMIT, an ORDER
// BY, or a UNION would change what a client receives while reading like a
// performance fix, so the shape is pinned here and the plan below is asserted
// against the statement read out of the query source, not a copy of it.
const wantReplayPredicate = "WHERE (admin_id = $1 OR participant_id = $1) AND date > $2;"

// secretChatsAfterDateSQL returns the replay statement exactly as the query
// source defines it, so the plan asserted below is the plan the server runs.
func secretChatsAfterDateSQL(tb testing.TB) string {
	tb.Helper()
	src, err := os.ReadFile(filepath.Join("queries", "secret_chats.sql"))
	if err != nil {
		tb.Fatalf("read secret chat query source: %v", err)
	}
	const marker = "-- name: SecretChatsAfterDate :many\n"
	i := strings.Index(string(src), marker)
	if i < 0 {
		tb.Fatal("SecretChatsAfterDate is missing from queries/secret_chats.sql")
	}
	stmt := string(src)[i+len(marker):]
	if j := strings.Index(stmt, "-- name:"); j >= 0 {
		stmt = stmt[:j]
	}
	stmt = strings.TrimSpace(stmt)
	if !strings.Contains(stmt, wantReplayPredicate) {
		tb.Fatalf("SecretChatsAfterDate no longer carries the fixed party-and-date predicate; got:\n%s", stmt)
	}
	for _, rewrite := range []string{"LIMIT", "ORDER BY", "UNION"} {
		if strings.Contains(strings.ToUpper(stmt), rewrite) {
			tb.Fatalf("SecretChatsAfterDate gained a %s rewrite; lifecycle replay is uncapped:\n%s", rewrite, stmt)
		}
	}
	return stmt
}

// explainNode is the subset of Postgres's JSON plan output this test reads.
type explainNode struct {
	NodeType            string        `json:"Node Type"`
	RelationName        string        `json:"Relation Name"`
	IndexName           string        `json:"Index Name"`
	IndexCond           string        `json:"Index Cond"`
	RowsRemovedByFilter int           `json:"Rows Removed by Filter"`
	SharedHitBlocks     int           `json:"Shared Hit Blocks"`
	SharedReadBlocks    int           `json:"Shared Read Blocks"`
	Plans               []explainNode `json:"Plans"`
}

// walk visits n and every node below it.
func (n explainNode) walk(visit func(explainNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// TestSecretChatsAfterDateUsesDateIndexes proves the lifecycle replay reads the
// changed set instead of a user's lifetime volume, and that it still returns
// exactly the right rows.
//
// The corpus is one account's 5,000 lifetime chats split across both party
// roles, 400 chats belonging to other accounts, and exactly 3 rows past the
// cursor. Without the two indexes the planner has nothing to use for either
// side of the OR and falls back to a Seq Scan of secret_chats: measured on this
// corpus that reads 62 shared blocks and discards 5,400 rows by filter, against
// 5 blocks and 0 rows with the indexes. That is the reason this test is untagged
// and runs in ordinary CI. The block ceiling sits deliberately above the
// observed cost, so it fails on a plan that stops being index-driven rather than
// on noise.
func TestSecretChatsAfterDateUsesDateIndexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	admin := mustCreateUser(t, s, "+15559970001")
	peer := mustCreateUser(t, s, "+15559970002")
	stranger := mustCreateUser(t, s, "+15559970003")

	// The cursor splits the corpus. Everything at or before it is a lifetime
	// row that predates the indexes; the 3 rows above it are the changed set a
	// poll must return. The lifetime rows sit one minute apart, so they span
	// about 83 hours below the cursor.
	cursor := time.Now().Add(-30 * time.Minute)
	const lifetime = 5000
	if _, err := conn.Exec(ctx, `
		INSERT INTO secret_chats (id, admin_id, participant_id, state, date)
		SELECT 500000 + g,
		       CASE WHEN g % 2 = 0 THEN $1::bigint ELSE $2::bigint END,
		       CASE WHEN g % 2 = 0 THEN $2::bigint ELSE $1::bigint END,
		       'discarded',
		       $3::timestamptz - make_interval(mins => g)
		FROM generate_series(1, $4::int) g`, admin, peer, cursor, lifetime); err != nil {
		t.Fatalf("seed lifetime chats: %v", err)
	}
	// Other accounts' rows keep the party predicate honest: an index-only read
	// path must not turn into a read path that returns what it did not select.
	if _, err := conn.Exec(ctx, `
		INSERT INTO secret_chats (id, admin_id, participant_id, state, date)
		SELECT 600000 + g, $1::bigint, $2::bigint, 'discarded', $3::timestamptz - make_interval(mins => g)
		FROM generate_series(1, 400) g`, peer, stranger, cursor); err != nil {
		t.Fatalf("seed other accounts' chats: %v", err)
	}
	// The changed set covers both roles: one chat the caller admins, two where
	// the caller is the participant.
	changed := []struct {
		id          int32
		adminID     int64
		participant int64
	}{
		{id: 700001, adminID: admin, participant: peer},
		{id: 700002, adminID: peer, participant: admin},
		{id: 700003, adminID: admin, participant: peer},
	}
	for i, c := range changed {
		if _, err := conn.Exec(ctx, `
			INSERT INTO secret_chats (id, admin_id, participant_id, state, date)
			VALUES ($1::int, $2::bigint, $3::bigint, 'active', $4::timestamptz + make_interval(mins => $5::int))`,
			c.id, c.adminID, c.participant, cursor, i+1); err != nil {
			t.Fatalf("seed changed chat %d: %v", c.id, err)
		}
	}
	if _, err := conn.Exec(ctx, `ANALYZE secret_chats`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	got, err := s.SecretChatsAfterDate(ctx, admin, cursor)
	if err != nil {
		t.Fatalf("secret chats after date: %v", err)
	}
	if len(got) != len(changed) {
		t.Fatalf("changed set = %d rows, want %d", len(got), len(changed))
	}
	want := map[int32]bool{700001: true, 700002: true, 700003: true}
	for _, c := range got {
		if !want[c.ID] {
			t.Errorf("changed set contains chat %d, which is not past the cursor", c.ID)
		}
		if !c.Party(admin) {
			t.Errorf("chat %d does not have the caller as a party: admin %d participant %d", c.ID, c.AdminID, c.ParticipantID)
		}
	}

	// The same query against a date older than every seeded row must still
	// return every qualifying pre-activation row: the indexes bound read work,
	// they do not narrow what a client that has been offline for a long time is
	// owed.
	all, err := s.SecretChatsAfterDate(ctx, admin, cursor.Add(-96*time.Hour))
	if err != nil {
		t.Fatalf("secret chats for an old cursor: %v", err)
	}
	if len(all) != lifetime+len(changed) {
		t.Errorf("old cursor returned %d rows, want %d", len(all), lifetime+len(changed))
	}
	for _, c := range all {
		if !c.Party(admin) {
			t.Fatalf("old cursor returned chat %d belonging to other accounts", c.ID)
		}
	}

	sql := secretChatsAfterDateSQL(t)
	root := explainRoot(t, ctx, conn, sql, admin, cursor)
	t.Logf("plan: %+v", root)

	blocks := root.SharedHitBlocks + root.SharedReadBlocks
	t.Logf("root shared blocks: hit %d read %d total %d", root.SharedHitBlocks, root.SharedReadBlocks, blocks)
	if blocks > 64 {
		t.Errorf("root read %d shared blocks for 3 changed rows out of %d, want at most 64", blocks, lifetime+len(changed)+400)
	}

	var removed int
	indexConds := map[string]string{}
	root.walk(func(n explainNode) {
		removed += n.RowsRemovedByFilter
		if n.IndexName != "" {
			indexConds[n.IndexName] = n.IndexCond
		}
	})
	for _, index := range []string{"secret_chats_admin_date_idx", "secret_chats_participant_date_idx"} {
		cond, ok := indexConds[index]
		if !ok {
			t.Errorf("plan does not read %s; it falls back to reading the whole table", index)
			continue
		}
		if !strings.Contains(cond, "date >") {
			t.Errorf("%s Index Cond does not carry the cursor bound: %s", index, cond)
		}
	}
	if removed > len(changed) {
		t.Errorf("plan discarded %d rows by filter, want at most the %d-row changed set", removed, len(changed))
	}
}

// explainRoot runs the replay statement under EXPLAIN (ANALYZE, BUFFERS,
// FORMAT JSON) and returns the root plan node, whose buffer counters cover the
// whole subtree.
func explainRoot(tb testing.TB, ctx context.Context, conn *pgx.Conn, query string, args ...any) explainNode {
	tb.Helper()
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw); err != nil {
		tb.Fatalf("explain replay query: %v", err)
	}
	var plans []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		tb.Fatalf("decode query plan: %v", err)
	}
	if len(plans) != 1 {
		tb.Fatalf("explain returned %d plans, want 1", len(plans))
	}
	return plans[0].Plan
}

func mustCreateUser(tb testing.TB, s *store.Store, phone string) int64 {
	tb.Helper()
	u, err := s.CreateUser(context.Background(), phone)
	if err != nil {
		tb.Fatalf("create user %s: %v", phone, err)
	}
	return u.ID
}

// TestSecretChatPartyDateIdxMigrationRollsBackAndRetries proves the migration's
// bounded-wait contract on disposable state: a build that cannot get the SHARE
// lock gives up at the lock timeout, and because the whole file runs in one
// transaction it leaves nothing behind, no index valid or INVALID, so the
// same file can be applied again once the lock is released. A CONCURRENTLY
// build would fail the other way, leaving an INVALID index the planner ignores
// silently and a migration recorded as applied.
func TestSecretChatPartyDateIdxMigrationRollsBackAndRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", dateIdxMigrationFile))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(body)
	for _, want := range []string{
		"SET LOCAL lock_timeout = '5s'",
		"SET LOCAL statement_timeout = '120s'",
		"CREATE INDEX secret_chats_admin_date_idx ON secret_chats (admin_id, date)",
		"CREATE INDEX secret_chats_participant_date_idx ON secret_chats (participant_id, date)",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("migration %s must contain %q", dateIdxMigrationFile, want)
		}
	}
	if strings.Contains(sql, "CREATE INDEX CONCURRENTLY") {
		t.Fatalf("migration %s must build inside its transaction, not CONCURRENTLY", dateIdxMigrationFile)
	}

	migs, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close

	name := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, pgtest.AdminDSN())
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
		if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("cleanup drop %s: %v", name, err)
		}
	})

	dsn := pgtest.DSNFrom(name)
	// Build the schema as of the migration before this one, so the file under
	// test is applied to the state a real deployment is in.
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("holder connect: %v", err)
	}
	defer func() { _ = holder.Close(ctx) }() //nolint:errcheck // best-effort close
	for _, entry := range migs {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= dateIdxMigrationFile {
			continue
		}
		prev, err := os.ReadFile(filepath.Join("..", "..", "migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if _, err := holder.Exec(ctx, string(prev)); err != nil {
			t.Fatalf("apply migration %s: %v", entry.Name(), err)
		}
	}
	if indexes := dateIndexes(ctx, t, holder); len(indexes) != 0 {
		t.Fatalf("date indexes present before the migration: %v", indexes)
	}

	migrator, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("migrator connect: %v", err)
	}
	defer func() { _ = migrator.Close(ctx) }() //nolint:errcheck // best-effort close

	// A writer holds ACCESS EXCLUSIVE on the table, which the build's SHARE
	// lock conflicts with, for longer than lock_timeout lets it wait.
	holderTx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder transaction: %v", err)
	}
	defer func() { _ = holderTx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op
	if _, err := holderTx.Exec(ctx, `LOCK TABLE secret_chats IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock secret_chats: %v", err)
	}
	if _, err := migrator.Exec(ctx, sql); err == nil {
		t.Fatal("the migration applied while the table was locked against its SHARE lock")
	} else if !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("migration failed for the wrong reason: %v", err)
	}
	if indexes := dateIndexes(ctx, t, migrator); len(indexes) != 0 {
		t.Fatalf("timed-out apply left indexes behind: %v", indexes)
	}
	if err := holderTx.Rollback(ctx); err != nil {
		t.Fatalf("release holder lock: %v", err)
	}

	// The same file applies cleanly afterwards, which is what a retried
	// migration run needs: the failed attempt recorded nothing to skip.
	if _, err := migrator.Exec(ctx, sql); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	indexes := dateIndexes(ctx, t, migrator)
	if len(indexes) != 2 {
		t.Fatalf("date indexes after retry = %v, want both", indexes)
	}
	for indexName, valid := range indexes {
		if !valid {
			t.Errorf("index %s is INVALID after the retry", indexName)
		}
	}
}

// dateIndexes returns every secret-chat date index by name, including an
// INVALID one, so a partially applied build cannot pass as applied.
func dateIndexes(ctx context.Context, tb testing.TB, conn *pgx.Conn) map[string]bool {
	tb.Helper()
	rows, err := conn.Query(ctx, `
		SELECT c.relname, i.indisvalid
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname IN ('secret_chats_admin_date_idx', 'secret_chats_participant_date_idx')`)
	if err != nil {
		tb.Fatalf("read date indexes: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var valid bool
		if err := rows.Scan(&name, &valid); err != nil {
			tb.Fatalf("scan date index: %v", err)
		}
		out[name] = valid
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("read date indexes: %v", err)
	}
	return out
}
