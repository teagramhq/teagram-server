package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	expiredCountLivePosts    = 1_000
	expiredCountExpiredPosts = 10_000
)

type expiredCountFixture struct {
	t     *testing.T
	s     *store.Store
	trace *historyCountTrace
	owner store.User
}

func newExpiredCountFixture(t *testing.T, phone string) *expiredCountFixture {
	t.Helper()
	ctx := context.Background()
	trace := &historyCountTrace{}
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(testBlobs(t)), store.WithQueryTracer(trace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return &expiredCountFixture{t: t, s: s, trace: trace, owner: mustUser(t, s, phone)}
}

func (f *expiredCountFixture) exec(query string, args ...any) {
	f.t.Helper()
	if err := store.ExecChannelPostSummarySQL(context.Background(), f.s, query, args...); err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
}

// seedTTLChannel creates a group whose posts 2..(1+live) expire in a day and
// posts after them expired an hour ago without being swept. Seeding bypasses
// the TTL stamp and summary triggers so expiry can be set directly; the
// summary nodes are then rebuilt by the explicit initializer.
func (f *expiredCountFixture) seedTTLChannel(title string, live, expired int) int64 {
	f.t.Helper()
	ctx := context.Background()
	group, err := f.s.CreateChannel(ctx, f.owner.ID, title, "", true)
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_summary_maintain`)
	f.exec(`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_stamp_history_ttl`)
	f.exec(`
		INSERT INTO channel_messages (channel_id, local_id, from_id, message, ttl_period, expires_at)
		SELECT $1, post_id, $2, 'seeded ttl post', 86400,
		       CASE WHEN post_id <= $3 THEN now() + interval '1 day'
		            ELSE now() - interval '1 hour' END
		  FROM generate_series(2::bigint, $4::bigint) AS post_id
	`, group.ID, f.owner.ID, live+1, live+expired+1)
	f.exec(`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`)
	f.exec(`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_stamp_history_ttl`)
	if err := f.s.InitializeChannelPostSummaries(ctx, group.ID); err != nil {
		f.t.Fatalf("initialize summaries: %v", err)
	}
	f.exec(`ANALYZE channel_messages`)
	f.exec(`ANALYZE channel_post_summaries`)
	return group.ID
}

// historyCost reads one history page and returns the number of statements it
// issued, the rows it read from channel_messages and the summary nodes, and
// the reported total count.
func (f *expiredCountFixture) historyCost(channelID int64) (statements int, rows int64, count int) {
	f.t.Helper()
	ctx := context.Background()
	f.trace.reset()
	_, count, err := f.s.ChannelHistoryWithOffset(ctx, channelID, f.owner.ID, 0, 0, 0, 0, 0, 100)
	if err != nil {
		f.t.Fatalf("read history: %v", err)
	}
	query, found := f.trace.historyCountQuery()
	if !found {
		f.t.Fatal("history count statement was not traced")
	}
	f.trace.mu.Lock()
	statements = len(f.trace.queries)
	f.trace.mu.Unlock()

	var raw []byte
	if err := store.StorePool(f.s).QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query.sql, query.args...).Scan(&raw); err != nil {
		f.t.Fatalf("explain history count: %v", err)
	}
	var explained []struct {
		Plan historyPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &explained); err != nil || len(explained) != 1 {
		f.t.Fatalf("decode history count plan: %v", err)
	}
	explained[0].Plan.walk(func(node historyPlanNode) {
		if node.RelationName == "channel_messages" || node.RelationName == "channel_post_summaries" {
			rows += int64(node.ActualRows * node.ActualLoops)
		}
	})
	return statements, rows, count
}

func TestChannelHistoryCountCostIgnoresExpiredUnsweptPosts(t *testing.T) {
	t.Parallel()
	f := newExpiredCountFixture(t, "+15551290001")
	clean := f.seedTTLChannel("TTL channel without expired posts", expiredCountLivePosts, 0)
	backlog := f.seedTTLChannel("TTL channel with unswept expired posts", expiredCountLivePosts, expiredCountExpiredPosts)

	cleanStatements, cleanRows, cleanCount := f.historyCost(clean)
	backlogStatements, backlogRows, backlogCount := f.historyCost(backlog)

	t.Logf("clean statements/rows = %d/%d, backlog statements/rows = %d/%d", cleanStatements, cleanRows, backlogStatements, backlogRows)

	// The service row at local ID 1 plus the live TTL posts; expired posts
	// never count.
	if want := expiredCountLivePosts + 1; cleanCount != want || backlogCount != want {
		t.Fatalf("history counts clean/backlog = %d/%d, want %d", cleanCount, backlogCount, want)
	}
	if float64(backlogStatements) > 1.5*float64(cleanStatements) {
		t.Fatalf("statements with %d expired unswept posts = %d, clean = %d, want within 1.5x", expiredCountExpiredPosts, backlogStatements, cleanStatements)
	}
	if float64(backlogRows) > 1.5*float64(cleanRows) {
		t.Fatalf("rows read with %d expired unswept posts = %d, clean = %d, want within 1.5x", expiredCountExpiredPosts, backlogRows, cleanRows)
	}
}

func TestChannelHistoryCountStaysExactAcrossFloorAndSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290002")
	// Posts 2..11 live, 12..31 expired unswept.
	channelID := f.seedTTLChannel("TTL floor channel", 10, 20)

	count := func() int {
		t.Helper()
		_, _, n := f.historyCost(channelID)
		return n
	}
	if got, want := count(), 11; got != want {
		t.Fatalf("count with no floor = %d, want %d", got, want)
	}

	// Floor inside the live range, and floors that split expired posts.
	for _, tc := range []struct {
		floor int64
		want  int
	}{
		{floor: 5, want: 6},   // live 6..11
		{floor: 11, want: 0},  // only expired posts above
		{floor: 20, want: 0},  // floor inside the expired range
		{floor: 100, want: 0}, // floor above every post
	} {
		f.exec(`UPDATE channel_participants SET available_min_id = $3 WHERE channel_id = $1 AND user_id = $2`,
			channelID, f.owner.ID, tc.floor)
		if got := count(); got != tc.want {
			t.Fatalf("count with floor %d = %d, want %d", tc.floor, got, tc.want)
		}
	}
	f.exec(`UPDATE channel_participants SET available_min_id = 0 WHERE channel_id = $1 AND user_id = $2`,
		channelID, f.owner.ID)

	// The sweeper tombstones expired posts; maintained nodes keep the count.
	f.exec(`UPDATE channel_messages SET deleted = true WHERE channel_id = $1 AND expires_at <= now()`, channelID)
	if got, want := count(), 11; got != want {
		t.Fatalf("count after sweep = %d, want %d", got, want)
	}
	// A post that expires after the summary was built leaves the count.
	f.exec(`UPDATE channel_messages SET expires_at = now() - interval '1 minute' WHERE channel_id = $1 AND local_id = 2`, channelID)
	if got, want := count(), 10; got != want {
		t.Fatalf("count after post expires = %d, want %d", got, want)
	}
	// Dropping the expiry must not change a live post's own count.
	f.exec(`UPDATE channel_messages SET expires_at = NULL WHERE channel_id = $1 AND local_id = 3`, channelID)
	if got, want := count(), 10; got != want {
		t.Fatalf("count after expiry removed = %d, want %d", got, want)
	}
	if err := f.s.InitializeChannelPostSummaries(ctx, channelID); err != nil {
		t.Fatalf("reinitialize summaries: %v", err)
	}
	if got, want := count(), 10; got != want {
		t.Fatalf("count after rebuild = %d, want %d", got, want)
	}
}

func TestChannelHistoryCountFallsBackUntilExpiringNodesAreBuilt(t *testing.T) {
	t.Parallel()
	f := newExpiredCountFixture(t, "+15551290003")
	channelID := f.seedTTLChannel("TTL fallback channel", 10, 20)

	// A channel created before the expiring scope existed is ready for the
	// older scopes only; history keeps counting expired rows directly.
	f.exec(`UPDATE channel_post_summary_state SET expiring_ready = false WHERE channel_id = $1`, channelID)
	f.exec(`DELETE FROM channel_post_summaries WHERE channel_id = $1 AND scope_kind = 2`, channelID)
	if _, _, got := f.historyCost(channelID); got != 11 {
		t.Fatalf("fallback count = %d, want 11", got)
	}
	f.exec(`UPDATE channel_messages SET expires_at = now() - interval '1 minute' WHERE channel_id = $1 AND local_id = 2`, channelID)
	if _, _, got := f.historyCost(channelID); got != 10 {
		t.Fatalf("fallback count after expiry = %d, want 10", got)
	}
}
