package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
	// lastMillis is the summed server execution time of the last historyCost.
	lastMillis float64
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
	f.lastMillis = 0
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
			Plan          historyPlanNode `json:"Plan"`
			ExecutionTime float64         `json:"Execution Time"`
		}
		if err := json.Unmarshal(raw, &explained); err != nil || len(explained) != 1 {
			f.t.Fatalf("decode history statement plan: %v", err)
		}
		f.lastMillis += explained[0].ExecutionTime
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

// expiredCountLiveSizes are the live TTL post counts at which the backlog
// independence bound is checked. History cost may grow with live TTL posts (the
// documented tradeoff); at every size it must not grow with the backlog.
var expiredCountLiveSizes = []int{expiredCountLivePosts, 10 * expiredCountLivePosts}

func TestChannelHistoryCountCostIgnoresExpiredUnsweptPosts(t *testing.T) {
	t.Parallel()
	f := newExpiredCountFixture(t, "+15551290001")
	for _, live := range expiredCountLiveSizes {
		clean := f.seedTTLChannel("TTL channel without expired posts", live, 0)
		backlog := f.seedTTLChannel("TTL channel with unswept expired posts", live, expiredCountExpiredPosts)

		cleanStatements, cleanRows, cleanCount, cleanIDs := f.historyCost(clean)
		cleanMillis := f.lastMillis
		backlogStatements, backlogRows, backlogCount, backlogIDs := f.historyCost(backlog)
		t.Logf("live %d: clean statements/rows/ms = %d/%d/%.1f, %d expired unswept statements/rows/ms = %d/%d/%.1f",
			live, cleanStatements, cleanRows, cleanMillis, expiredCountExpiredPosts, backlogStatements, backlogRows, f.lastMillis)

		if len(backlogIDs) != 100 || !slices.Equal(backlogIDs, cleanIDs) {
			t.Fatalf("live %d: page with expired unswept posts = %v, want the clean channel's newest live page %v", live, backlogIDs, cleanIDs)
		}
		for _, id := range backlogIDs {
			if id > int64(live)+1 {
				t.Fatalf("live %d: page contains expired post %d", live, id)
			}
		}
		// The service row at local ID 1 plus the live TTL posts; expired posts
		// never count.
		if want := live + 1; cleanCount != want || backlogCount != want {
			t.Fatalf("live %d: history counts clean/backlog = %d/%d, want %d", live, cleanCount, backlogCount, want)
		}
		if float64(backlogStatements) > 1.5*float64(cleanStatements) {
			t.Fatalf("live %d: statements with %d expired unswept posts = %d, clean = %d, want within 1.5x", live, expiredCountExpiredPosts, backlogStatements, cleanStatements)
		}
		if float64(backlogRows) > 1.5*float64(cleanRows) {
			t.Fatalf("live %d: rows read with %d expired unswept posts = %d, clean = %d, want within 1.5x", live, expiredCountExpiredPosts, backlogRows, cleanRows)
		}
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

func TestChannelHistoryCountTracksTTLPostsWrittenThroughNormalPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290005")
	group, err := f.s.CreateChannel(ctx, f.owner.ID, "TTL normal path", "", true)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		_, _, n, _ := f.historyCost(group.ID)
		return n
	}
	before := count()
	if _, _, _, changed, err := f.s.SetChannelHistoryTTL(ctx, group.ID, f.owner.ID, 86400, store.RateLimitConfig{}); err != nil || !changed {
		t.Fatalf("set channel history TTL: changed=%v err=%v", changed, err)
	}
	serviceRows := count() // the TTL-change service row, no TTL posts yet
	if serviceRows != before+1 {
		t.Fatalf("count after TTL change = %d, want %d", serviceRows, before+1)
	}
	postIDs := make([]int64, 0, 3)
	for i := range int64(3) {
		msg, _ := post(t, f.s, group.ID, f.owner.ID, "ttl post", 90_000+i)
		postIDs = append(postIDs, msg.LocalID)
	}
	if got, want := count(), serviceRows+3; got != want {
		t.Fatalf("count with 3 TTL posts = %d, want %d", got, want)
	}
	var root int64
	if err := store.StorePool(f.s).QueryRow(ctx, `
		SELECT live_count FROM channel_post_summaries
		 WHERE channel_id = $1 AND scope_kind = 2 AND author_id = 0 AND depth = 0 AND prefix = 0
	`, group.ID).Scan(&root); err != nil || root != 3 {
		t.Fatalf("expiring root after TTL posts = %d, err %v, want 3", root, err)
	}

	f.exec(`UPDATE channel_messages SET expires_at = now() - interval '1 minute' WHERE channel_id = $1 AND local_id = $2`, group.ID, postIDs[0])
	if got, want := count(), serviceRows+2; got != want {
		t.Fatalf("count after one post expires = %d, want %d", got, want)
	}
	f.exec(`UPDATE channel_messages SET deleted = true WHERE channel_id = $1 AND local_id = $2`, group.ID, postIDs[0])
	if got, want := count(), serviceRows+2; got != want {
		t.Fatalf("count after the sweeper tombstones it = %d, want %d", got, want)
	}
	if err := store.StorePool(f.s).QueryRow(ctx, `
		SELECT live_count FROM channel_post_summaries
		 WHERE channel_id = $1 AND scope_kind = 2 AND author_id = 0 AND depth = 0 AND prefix = 0
	`, group.ID).Scan(&root); err != nil || root != 2 {
		t.Fatalf("expiring root after sweep = %d, err %v, want 2", root, err)
	}
}

func TestChannelHistoryCountFailsClosedWhenExpiringNodesAreMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290006")
	channelID := f.seedTTLChannel("TTL corrupt channel", 5, 5)
	f.exec(`DELETE FROM channel_post_summaries WHERE channel_id = $1 AND scope_kind = 2 AND depth = 0 AND prefix = 0`, channelID)
	_, _, err := f.s.ChannelHistoryWithOffset(ctx, channelID, f.owner.ID, 0, 0, 0, 0, 0, 100)
	if !errors.Is(err, store.ErrChannelPostSummaryCorrupt) {
		t.Fatalf("history with a missing expiring root = %v, want ErrChannelPostSummaryCorrupt", err)
	}
}

func (f *expiredCountFixture) expiringReady(channelID int64) bool {
	f.t.Helper()
	var ready bool
	if err := store.StorePool(f.s).QueryRow(context.Background(),
		`SELECT expiring_ready FROM channel_post_summary_state WHERE channel_id = $1`, channelID).Scan(&ready); err != nil {
		f.t.Fatalf("read expiring readiness: %v", err)
	}
	return ready
}

// pendingExpiringChannel is a channel that predates the expiring scope: ready
// for the older scopes, with live and expired TTL posts and no scope-2 nodes.
func (f *expiredCountFixture) pendingExpiringChannel(title string) int64 {
	f.t.Helper()
	channelID := f.seedTTLChannel(title, 10, 20)
	f.exec(`UPDATE channel_post_summary_state SET expiring_ready = false WHERE channel_id = $1`, channelID)
	f.exec(`DELETE FROM channel_post_summaries WHERE channel_id = $1 AND scope_kind = 2`, channelID)
	f.exec(`UPDATE channels SET ttl_period = 86400 WHERE id = $1`, channelID)
	return channelID
}

func TestChannelPostExpiringSummaryRebuildIsAtomicAndRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290007")
	channelID := f.pendingExpiringChannel("TTL failing rebuild")

	f.exec(`ALTER TABLE channel_post_summaries ADD CONSTRAINT channel_post_expiring_test_fail CHECK (false) NOT VALID`)
	initialized, remaining, err := f.s.InitializeChannelPostExpiringSummaries(ctx, 10)
	if err == nil {
		t.Fatal("rebuild succeeded with a failing derived-row constraint")
	}
	if initialized != 0 || remaining != 1 {
		t.Fatalf("failed run initialized/remaining = %d/%d, want 0/1 (error %v)", initialized, remaining, err)
	}
	if f.expiringReady(channelID) {
		t.Fatal("expiring_ready is set after a failed rebuild")
	}
	var nodes int
	if err := store.StorePool(f.s).QueryRow(ctx,
		`SELECT count(*) FROM channel_post_summaries WHERE channel_id = $1 AND scope_kind = 2`, channelID).Scan(&nodes); err != nil || nodes != 0 {
		t.Fatalf("expiring nodes after failed rebuild = %d, err %v, want 0", nodes, err)
	}
	if _, _, got, _ := f.historyCost(channelID); got != 11 {
		t.Fatalf("count after failed rebuild = %d, want 11 from the fallback", got)
	}

	f.exec(`ALTER TABLE channel_post_summaries DROP CONSTRAINT channel_post_expiring_test_fail`)
	initialized, remaining, err = f.s.InitializeChannelPostExpiringSummaries(ctx, 10)
	if err != nil || initialized != 1 || remaining != 0 {
		t.Fatalf("retry initialized/remaining = %d/%d, err %v, want 1/0", initialized, remaining, err)
	}
	if !f.expiringReady(channelID) {
		t.Fatal("expiring_ready is unset after a successful rebuild")
	}
	if _, _, got, _ := f.historyCost(channelID); got != 11 {
		t.Fatalf("count after retry = %d, want 11", got)
	}
}

func TestChannelPostExpiringSummaryRebuildSerializesWithTTLPosts(t *testing.T) {
	t.Parallel()
	for _, rebuildFirst := range []bool{true, false} {
		name := "writer queued first"
		if rebuildFirst {
			name = "rebuild queued first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			phone := "+15551290008"
			if rebuildFirst {
				phone = "+15551290009"
			}
			f := newExpiredCountFixture(t, phone)
			channelID := f.pendingExpiringChannel("TTL rebuild race")

			hold, err := store.BeginChannelPostSummaryStateHold(ctx, f.s)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hold.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
			if _, err := hold.Exec(ctx, `SELECT channel_id FROM channel_state WHERE channel_id = $1 FOR UPDATE`, channelID); err != nil {
				t.Fatalf("hold channel state: %v", err)
			}

			rebuildDone := make(chan error, 1)
			writeDone := make(chan error, 1)
			startRebuild := func() {
				go func() {
					_, _, rebuildErr := f.s.InitializeChannelPostExpiringSummaries(ctx, 10)
					rebuildDone <- rebuildErr
				}()
			}
			startWrite := func() {
				go func() {
					writeDone <- store.ExecChannelPostSummarySQL(ctx, f.s, `
						INSERT INTO channel_messages (channel_id, local_id, from_id, message)
						VALUES ($1, 1000, $2, 'ttl post during rebuild')
					`, channelID, f.owner.ID)
				}()
			}
			first, second := startWrite, startRebuild
			if rebuildFirst {
				first, second = startRebuild, startWrite
			}
			first()
			if err := store.WaitForLockWaiters(ctx, f.s, 1); err != nil {
				t.Fatalf("wait for first queued transaction: %v", err)
			}
			second()
			if err := store.WaitForLockWaiters(ctx, f.s, 2); err != nil {
				t.Fatalf("wait for second queued transaction: %v", err)
			}
			if err := hold.Rollback(ctx); err != nil {
				t.Fatalf("release channel state: %v", err)
			}
			if err := <-rebuildDone; err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			if err := <-writeDone; err != nil {
				t.Fatalf("write TTL post: %v", err)
			}

			if !f.expiringReady(channelID) {
				t.Fatal("expiring_ready is unset after the rebuild")
			}
			var root int64
			if err := store.StorePool(f.s).QueryRow(ctx, `
				SELECT live_count FROM channel_post_summaries
				 WHERE channel_id = $1 AND scope_kind = 2 AND author_id = 0 AND depth = 0 AND prefix = 0
			`, channelID).Scan(&root); err != nil {
				t.Fatalf("read expiring root: %v", err)
			}
			// 10 live and 20 expired TTL posts seeded, plus the racing post.
			if root != 31 {
				t.Fatalf("expiring root = %d, want 31 (the racing post counted exactly once)", root)
			}
			if _, _, got, _ := f.historyCost(channelID); got != 12 {
				t.Fatalf("history count = %d, want 12", got)
			}
		})
	}
}

func TestChannelPostExpiringSummaryPendingReportsUnrebuiltChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290010")
	f.pendingExpiringChannel("TTL pending one")
	f.pendingExpiringChannel("TTL pending two")
	if pending, err := f.s.ChannelPostExpiringSummaryPending(ctx); err != nil || pending != 2 {
		t.Fatalf("pending = %d, err %v, want 2", pending, err)
	}
	if _, remaining, err := f.s.InitializeChannelPostExpiringSummaries(ctx, 10); err != nil || remaining != 0 {
		t.Fatalf("remaining = %d, err %v, want 0", remaining, err)
	}
	if pending, err := f.s.ChannelPostExpiringSummaryPending(ctx); err != nil || pending != 0 {
		t.Fatalf("pending after rollout = %d, err %v, want 0", pending, err)
	}
}
