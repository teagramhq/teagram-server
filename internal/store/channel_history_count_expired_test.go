package store_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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
// issued, the rows every traced history statement read from channel_messages
// and the summary nodes (measured by replaying each one under EXPLAIN
// ANALYZE), the reported total count and the IDs of the returned page.
func (f *expiredCountFixture) historyCost(channelID int64) (statements int, rows int64, count int, pageIDs []int64) {
	f.t.Helper()
	ctx := context.Background()
	f.trace.reset()
	page, count, err := f.s.ChannelHistoryWithOffset(ctx, channelID, f.owner.ID, 0, 0, 0, 0, 0, 100)
	if err != nil {
		f.t.Fatalf("read history: %v", err)
	}
	for _, msg := range page {
		pageIDs = append(pageIDs, msg.LocalID)
	}
	f.trace.mu.Lock()
	traced := append([]historyCountQuery(nil), f.trace.queries...)
	f.trace.mu.Unlock()
	statements = len(traced)

	var named int
	for _, query := range traced {
		if !strings.Contains(query.sql, "-- name:") {
			continue
		}
		named++
		var raw []byte
		if err := store.StorePool(f.s).QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query.sql, query.args...).Scan(&raw); err != nil {
			f.t.Fatalf("explain history statement: %v", err)
		}
		var explained []struct {
			Plan historyPlanNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &explained); err != nil || len(explained) != 1 {
			f.t.Fatalf("decode history statement plan: %v", err)
		}
		explained[0].Plan.walk(func(node historyPlanNode) {
			if node.RelationName == "channel_messages" || node.RelationName == "channel_post_summaries" {
				rows += int64(node.ActualRows * node.ActualLoops)
			}
		})
	}
	if named < 2 {
		f.t.Fatalf("traced %d named history statements, want the count and the page", named)
	}
	return statements, rows, count, pageIDs
}

func TestChannelHistoryCountCostIgnoresExpiredUnsweptPosts(t *testing.T) {
	t.Parallel()
	f := newExpiredCountFixture(t, "+15551290001")
	clean := f.seedTTLChannel("TTL channel without expired posts", expiredCountLivePosts, 0)
	backlog := f.seedTTLChannel("TTL channel with unswept expired posts", expiredCountLivePosts, expiredCountExpiredPosts)

	cleanStatements, cleanRows, cleanCount, cleanIDs := f.historyCost(clean)
	backlogStatements, backlogRows, backlogCount, backlogIDs := f.historyCost(backlog)

	t.Logf("clean statements/rows = %d/%d, backlog statements/rows = %d/%d", cleanStatements, cleanRows, backlogStatements, backlogRows)

	if len(backlogIDs) != 100 || !slices.Equal(backlogIDs, cleanIDs) {
		t.Fatalf("page with expired unswept posts = %v, want the clean channel's newest live page %v", backlogIDs, cleanIDs)
	}
	for _, id := range backlogIDs {
		if id > expiredCountLivePosts+1 {
			t.Fatalf("page contains expired post %d", id)
		}
	}

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
		_, _, n, ids := f.historyCost(channelID)
		for _, id := range ids {
			if id > 11 {
				t.Fatalf("page contains expired post %d", id)
			}
		}
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
	if _, _, got, _ := f.historyCost(channelID); got != 11 {
		t.Fatalf("fallback count = %d, want 11", got)
	}
	f.exec(`UPDATE channel_messages SET expires_at = now() - interval '1 minute' WHERE channel_id = $1 AND local_id = 2`, channelID)
	if _, _, got, _ := f.historyCost(channelID); got != 10 {
		t.Fatalf("fallback count after expiry = %d, want 10", got)
	}
}

func TestChannelPostExpiringSummaryBackfillIsBoundedAndComplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290004")
	first := f.seedTTLChannel("TTL backfill first", 10, 2_000)
	second := f.seedTTLChannel("TTL backfill second", 5, 2_000)

	// Channels that predate the expiring scope: ready for the old scopes only.
	f.exec(`UPDATE channel_post_summary_state SET expiring_ready = false`)
	f.exec(`DELETE FROM channel_post_summaries WHERE scope_kind = 2`)
	_, slowRows, count, _ := f.historyCost(first)
	if count != 11 {
		t.Fatalf("pre-backfill count = %d, want 11", count)
	}
	if slowRows < 2_000 {
		t.Fatalf("pre-backfill history read %d rows, want the direct expired-row scan", slowRows)
	}

	if _, _, err := f.s.InitializeChannelPostExpiringSummaries(ctx, 0); err == nil {
		t.Fatal("batch size 0 succeeded, want an error")
	}
	initialized, remaining, err := f.s.InitializeChannelPostExpiringSummaries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if initialized != 1 || remaining != 1 {
		t.Fatalf("first batch initialized/remaining = %d/%d, want 1/1", initialized, remaining)
	}
	initialized, remaining, err = f.s.InitializeChannelPostExpiringSummaries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if initialized != 1 || remaining != 0 {
		t.Fatalf("second batch initialized/remaining = %d/%d, want 1/0", initialized, remaining)
	}
	// Completed rollout is idempotent.
	if initialized, remaining, err = f.s.InitializeChannelPostExpiringSummaries(ctx, 10); err != nil || initialized != 0 || remaining != 0 {
		t.Fatalf("rerun initialized/remaining = %d/%d, err %v, want 0/0", initialized, remaining, err)
	}

	for channelID, wantCount := range map[int64]int{first: 11, second: 6} {
		_, rows, count, ids := f.historyCost(channelID)
		if count != wantCount {
			t.Fatalf("channel %d count after backfill = %d, want %d", channelID, count, wantCount)
		}
		if rows >= 1_000 {
			t.Fatalf("channel %d history read %d rows after backfill, want no expired-row scan", channelID, rows)
		}
		for _, id := range ids {
			if id > int64(wantCount) {
				t.Fatalf("channel %d page contains expired post %d", channelID, id)
			}
		}
	}
}
