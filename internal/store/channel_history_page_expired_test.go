package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// The history page statement walks past expired unswept posts only as far as
// the smaller of the backlog and the live expiring posts: its rows examined
// (returned plus filtered or rechecked) stay within 1.5x of the same channel
// without a backlog plus min(backlog, live), whether the expired posts are
// newer or older than the visible window, and the page is the clean channel's.
func TestChannelHistoryPageCostIgnoresExpiredUnsweptPosts(t *testing.T) {
	t.Parallel()
	f := newExpiredCountFixture(t, "+15551290021")
	for _, expiredOldest := range []bool{false, true} {
		for _, live := range []int{50, 1_000, 10_000} {
			clean := f.seedTTLChannelLayout("TTL channel without expired posts", live, 0, expiredOldest)
			backlog := f.seedTTLChannelLayout("TTL channel with unswept expired posts", live, expiredCountExpiredPosts, expiredOldest)

			cleanStatements, _, _, cleanIDs := f.historyCost(clean)
			cleanPageRows := f.lastPageRows
			backlogStatements, _, _, backlogIDs := f.historyCost(backlog)
			backlogPageRows := f.lastPageRows
			bound := cleanPageRows + int64(min(expiredCountExpiredPosts, live))
			t.Logf("live %d, expired oldest %v: page statement rows %d vs %d with %d expired unswept (bound %d)",
				live, expiredOldest, cleanPageRows, backlogPageRows, expiredCountExpiredPosts, bound)

			if expiredOldest {
				// The live posts follow the expired ones, so their IDs are
				// shifted; the service row at local ID 1 is not.
				for i, id := range backlogIDs {
					if id > 1 {
						backlogIDs[i] = id - expiredCountExpiredPosts
					}
				}
			}
			if !slices.Equal(backlogIDs, cleanIDs) {
				t.Fatalf("live %d, expired oldest %v: page = %v, want the clean channel's page %v", live, expiredOldest, backlogIDs, cleanIDs)
			}
			if backlogStatements != cleanStatements {
				t.Fatalf("live %d, expired oldest %v: statements with backlog = %d, no backlog = %d, want equal", live, expiredOldest, backlogStatements, cleanStatements)
			}
			if float64(backlogPageRows) > 1.5*float64(bound) {
				t.Fatalf("live %d, expired oldest %v: page statement rows with backlog = %d, want within 1.5x of %d (no-backlog %d + min(backlog, live))",
					live, expiredOldest, backlogPageRows, bound, cleanPageRows)
			}
		}
	}
}

// The same bound holds for a page below a non-zero offset_id, the cursor every
// scroll back through history sends, wherever the expired posts sit relative to
// it, and the page equals the clean channel's page below the same post.
func TestChannelHistoryPageCostIgnoresExpiredUnsweptPostsBelowOffset(t *testing.T) {
	t.Parallel()
	f := newExpiredCountFixture(t, "+15551290051")
	for _, expiredOldest := range []bool{false, true} {
		for _, live := range []int{50, 1_000, 10_000} {
			clean := f.seedTTLChannelLayout("TTL channel without expired posts", live, 0, expiredOldest)
			backlog := f.seedTTLChannelLayout("TTL channel with unswept expired posts", live, expiredCountExpiredPosts, expiredOldest)
			// Below the middle live post. The live posts follow the expired
			// ones when expiredOldest, so their IDs are shifted in the backlog.
			cleanOffset := int64(live/2 + 2)
			backlogOffset := cleanOffset
			if expiredOldest {
				backlogOffset += expiredCountExpiredPosts
			}

			cleanStatements, _, _, cleanIDs := f.historyCostAt(clean, cleanOffset)
			cleanPageRows := f.lastPageRows
			backlogStatements, _, _, backlogIDs := f.historyCostAt(backlog, backlogOffset)
			backlogPageRows := f.lastPageRows
			bound := cleanPageRows + int64(min(expiredCountExpiredPosts, live))
			t.Logf("live %d, expired oldest %v, offset_id set: page statement rows %d vs %d with %d expired unswept (bound %d)",
				live, expiredOldest, cleanPageRows, backlogPageRows, expiredCountExpiredPosts, bound)

			if expiredOldest {
				for i, id := range backlogIDs {
					if id > 1 {
						backlogIDs[i] = id - expiredCountExpiredPosts
					}
				}
			}
			if len(cleanIDs) == 0 || !slices.Equal(backlogIDs, cleanIDs) {
				t.Fatalf("live %d, expired oldest %v: page below offset = %v, want the clean channel's page %v", live, expiredOldest, backlogIDs, cleanIDs)
			}
			if backlogStatements != cleanStatements {
				t.Fatalf("live %d, expired oldest %v: statements with backlog = %d, no backlog = %d, want equal", live, expiredOldest, backlogStatements, cleanStatements)
			}
			if float64(backlogPageRows) > 1.5*float64(bound) {
				t.Fatalf("live %d, expired oldest %v: page statement rows below offset with backlog = %d, want within 1.5x of %d (no-backlog %d + min(backlog, live))",
					live, expiredOldest, backlogPageRows, bound, cleanPageRows)
			}
		}
	}
}

// Both page statements return the same rows for every floor, offset, window
// and bound, over a channel mixing every row class the statements partition:
// posts with no deadline, unexpired and expired expiring posts, service rows
// with and without a TTL policy and deleted rows of each, for members with
// every floor, non-members, banned members and deleted channels.
func TestChannelHistoryPageUnexpiredMatchesWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290031")
	group, err := f.s.CreateChannel(ctx, f.owner.ID, "mixed history", "", true)
	if err != nil {
		t.Fatal(err)
	}
	const posts = 300
	f.exec(`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_summary_maintain`)
	f.exec(`ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_stamp_history_ttl`)
	f.exec(`
		INSERT INTO channel_messages (channel_id, local_id, from_id, message, deleted, action_type, ttl_period, expires_at)
		SELECT $1, post_id, $2, 'seeded post',
		       post_id % 7 IN (3, 6),
		       CASE WHEN post_id % 7 IN (4, 5) THEN 1 ELSE 0 END,
		       CASE WHEN post_id % 7 IN (1, 2, 5, 6) THEN 86400 ELSE 0 END,
		       CASE post_id % 7
		            WHEN 1 THEN now() + interval '1 day'
		            WHEN 6 THEN now() + interval '1 day'
		            WHEN 2 THEN now() - interval '1 hour'
		       END
		  FROM generate_series(2::bigint, $3::bigint) AS post_id
	`, group.ID, f.owner.ID, int64(posts))
	f.exec(`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`)
	f.exec(`ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_stamp_history_ttl`)
	f.exec(`ANALYZE channel_messages`)

	var unexpired int64
	if err := store.StorePool(f.s).QueryRow(ctx, `
		SELECT count(*) FROM channel_messages
		 WHERE channel_id = $1 AND NOT deleted AND action_type = 0
		   AND expires_at IS NOT NULL AND expires_at > now()
	`, group.ID).Scan(&unexpired); err != nil || unexpired == 0 {
		t.Fatalf("unexpired expiring posts = %d, err %v", unexpired, err)
	}
	q := db.New(store.StorePool(f.s))
	stranger := mustUser(t, f.s, "+15551290032")

	compared := 0
	compare := func(owner, floor, offsetID, addOffset, maxID, minID int64, lim int32) {
		t.Helper()
		walk, err := q.ChannelHistoryPageWithOffset(ctx, db.ChannelHistoryPageWithOffsetParams{
			ChannelID: group.ID, OwnerID: owner, OffsetID: offsetID, AddOffset: addOffset, MaxID: maxID, MinID: minID, Lim: lim,
		})
		if err != nil {
			t.Fatal(err)
		}
		skip, err := q.ChannelHistoryPageUnexpired(ctx, db.ChannelHistoryPageUnexpiredParams{
			ChannelID: group.ID, OwnerID: owner, OffsetID: offsetID, AddOffset: addOffset, MaxID: maxID, MinID: minID, Lim: lim,
		})
		if err != nil {
			t.Fatal(err)
		}
		walkIDs := make([]int64, len(walk))
		for i, row := range walk {
			walkIDs[i] = row.LocalID
		}
		skipIDs := make([]int64, len(skip))
		for i, row := range skip {
			skipIDs[i] = row.LocalID
		}
		if !slices.Equal(walkIDs, skipIDs) {
			t.Fatalf("owner %d floor %d offset %d add %d max %d min %d lim %d: unexpired page %v, walk page %v",
				owner, floor, offsetID, addOffset, maxID, minID, lim, skipIDs, walkIDs)
		}
		compared++
	}
	compareAround := func(owner, floor, offsetID, addOffset, maxID, minID int64, lim int32) {
		t.Helper()
		walk, err := q.ChannelHistoryPageAround(ctx, db.ChannelHistoryPageAroundParams{
			ChannelID: group.ID, OwnerID: owner, OffsetID: offsetID, AddOffset: addOffset, MaxID: maxID, MinID: minID, Lim: lim,
		})
		if err != nil {
			t.Fatal(err)
		}
		unexpired, err := q.ChannelHistoryPageAroundUnexpired(ctx, db.ChannelHistoryPageAroundUnexpiredParams{
			ChannelID: group.ID, OwnerID: owner, OffsetID: offsetID, AddOffset: addOffset, MaxID: maxID, MinID: minID, Lim: lim,
		})
		if err != nil {
			t.Fatal(err)
		}
		walkIDs := make([]int64, len(walk))
		for i, row := range walk {
			walkIDs[i] = row.LocalID
		}
		unexpiredIDs := make([]int64, len(unexpired))
		for i, row := range unexpired {
			unexpiredIDs[i] = row.LocalID
		}
		if !slices.Equal(walkIDs, unexpiredIDs) {
			t.Fatalf("owner %d floor %d offset %d add %d max %d min %d lim %d: unexpired around page %v, walk page %v",
				owner, floor, offsetID, addOffset, maxID, minID, lim, unexpiredIDs, walkIDs)
		}
		compared++
	}
	// Without summary nodes the statement drops its bound and reads every
	// unexpired post; with them it is bounded by the summary total. A
	// contradictory summary (more expired than total) also drops the bound.
	for _, mode := range []string{"no summary nodes", "initialized summary nodes", "contradictory summary nodes"} {
		switch mode {
		case "no summary nodes":
			// New channels start ready; the seeded rows bypassed the triggers,
			// so mark the nodes as not yet built, as on an existing channel.
			f.exec(`UPDATE channel_post_summary_state SET expiring_ready = false WHERE channel_id = $1`, group.ID)
		case "initialized summary nodes":
			if err := f.s.InitializeChannelPostSummaries(ctx, group.ID); err != nil {
				t.Fatalf("initialize summaries: %v", err)
			}
		case "contradictory summary nodes":
			f.exec(`UPDATE channel_post_summaries SET live_count = 0 WHERE channel_id = $1 AND scope_kind = 2 AND depth = 0 AND prefix = 0`, group.ID)
		}
		for _, owner := range []int64{f.owner.ID, stranger.ID} {
			for _, floor := range []int64{0, 50, 299} {
				f.exec(`UPDATE channel_participants SET available_min_id = $3 WHERE channel_id = $1 AND user_id = $2`, group.ID, owner, floor)
				for _, offsetID := range []int64{0, 5, 150, 400} {
					for _, addOffset := range []int64{0, 3, 40} {
						for _, maxID := range []int64{0, 200} {
							for _, minID := range []int64{0, 20} {
								for _, lim := range []int32{1, 10, 100} {
									compare(owner, floor, offsetID, addOffset, maxID, minID, lim)
								}
							}
						}
					}
				}
				for _, offsetID := range []int64{5, 150, 400} {
					for _, addOffset := range []int64{-1, -20} {
						for _, window := range [][2]int64{{0, 0}, {200, 20}} {
							for _, lim := range []int32{1, 10} {
								compareAround(owner, floor, offsetID, addOffset, window[0], window[1], lim)
							}
						}
					}
				}
			}
		}

	}

	f.exec(`DELETE FROM channel_post_summaries WHERE channel_id = $1`, group.ID)
	f.exec(`UPDATE channel_post_summary_state SET expiring_ready = false WHERE channel_id = $1`, group.ID)
	if err := f.s.InitializeChannelPostSummaries(ctx, group.ID); err != nil {
		t.Fatalf("reinitialize summaries: %v", err)
	}
	// Access state decides visibility in both statements: an active ban, a
	// channel deleted after the page was last read, and a lapsed ban must
	// agree. The two denied states must return nothing, so a statement that
	// ignored them cannot pass as "equal and empty" by accident.
	f.exec(`UPDATE channel_participants SET available_min_id = 0 WHERE channel_id = $1 AND user_id = $2`, group.ID, f.owner.ID)
	for _, state := range []struct {
		name   string
		setup  string
		denied bool
	}{
		{"active ban", `UPDATE channel_participants SET banned_until = now() + interval '1 hour' WHERE channel_id = $1 AND user_id = $2`, true},
		{"lapsed ban", `UPDATE channel_participants SET banned_until = now() - interval '1 hour' WHERE channel_id = $1 AND user_id = $2`, false},
		{"deleted channel", `UPDATE channel_participants SET banned_until = NULL WHERE channel_id = $1 AND user_id = $2; UPDATE channels SET deleted_at = now() WHERE id = $1`, true},
	} {
		for stmt := range strings.SplitSeq(state.setup, "; ") {
			if strings.HasPrefix(stmt, "UPDATE channels") {
				f.exec(stmt, group.ID)
			} else {
				f.exec(stmt, group.ID, f.owner.ID)
			}
		}
		for _, offsetID := range []int64{0, 150} {
			for _, addOffset := range []int64{0, 3} {
				compare(f.owner.ID, 0, offsetID, addOffset, 0, 0, 10)
			}
		}
		for _, addOffset := range []int64{-3, -40} {
			compareAround(f.owner.ID, 0, 150, addOffset, 0, 0, 10)
		}
		rows, err := q.ChannelHistoryPageUnexpired(ctx, db.ChannelHistoryPageUnexpiredParams{
			ChannelID: group.ID, OwnerID: f.owner.ID, Lim: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if state.denied && len(rows) != 0 {
			t.Fatalf("%s: unexpired page returned %d rows, want none", state.name, len(rows))
		}
		if !state.denied && len(rows) == 0 {
			t.Fatalf("%s: unexpired page returned no rows, want the member's page", state.name)
		}
		around, err := q.ChannelHistoryPageAroundUnexpired(ctx, db.ChannelHistoryPageAroundUnexpiredParams{
			ChannelID: group.ID, OwnerID: f.owner.ID, OffsetID: 150, AddOffset: -3, Lim: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if state.denied && len(around) != 0 {
			t.Fatalf("%s: unexpired around page returned %d rows, want none", state.name, len(around))
		}
		if !state.denied && len(around) == 0 {
			t.Fatalf("%s: unexpired around page returned no rows, want the member's page", state.name)
		}
	}
	if compared == 0 {
		t.Fatal("compared no pages")
	}
}

// A group with no expiring posts, such as every group without a TTL policy,
// never takes the unexpired-post read: its backlog is not larger than zero
// unexpired expiring posts, so the page comes from the primary-key walk.
func TestChannelHistoryPageUsesWalkWithoutBacklog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newExpiredCountFixture(t, "+15551290041")
	group, err := f.s.CreateChannel(ctx, f.owner.ID, "plain group", "", true)
	if err != nil {
		t.Fatal(err)
	}
	f.trace.reset()
	if _, _, err := f.s.ChannelHistoryWithOffset(ctx, group.ID, f.owner.ID, 0, 0, 0, 0, 0, 100); err != nil {
		t.Fatal(err)
	}
	f.trace.mu.Lock()
	defer f.trace.mu.Unlock()
	var walk, unexpired int
	for _, query := range f.trace.queries {
		switch {
		case strings.Contains(query.sql, "-- name: ChannelHistoryPageWithOffset"):
			walk++
		case strings.Contains(query.sql, "-- name: ChannelHistoryPageUnexpired"):
			unexpired++
		}
	}
	if walk != 1 || unexpired != 0 {
		t.Fatalf("page statements: walk %d, unexpired %d, want 1 and 0", walk, unexpired)
	}
}

// The around page's rows examined stay within 1.5x of the same channel without
// an expired backlog plus the smaller of the backlog and the live expiring
// posts. The page and statement count are unchanged whether the expired posts
// are newer or older than the target.
func TestChannelHistoryAroundCostIgnoresExpiredUnsweptPosts(t *testing.T) {
	t.Parallel()
	f := newExpiredCountFixture(t, "+15551290061")
	assertAroundQuery := func(want string) {
		t.Helper()
		f.trace.mu.Lock()
		defer f.trace.mu.Unlock()
		for _, query := range f.trace.queries {
			if strings.Contains(query.sql, "-- name: ChannelHistoryPageAround") {
				if strings.Contains(query.sql, "-- name: "+want+" ") {
					return
				}
				t.Fatalf("around page query = %q, want %s", query.sql, want)
			}
		}
		t.Fatalf("around page query %s was not traced", want)
	}
	for _, expiredOldest := range []bool{false, true} {
		for _, live := range []int{50, 1_000, 10_000} {
			clean := f.seedTTLChannelLayout("TTL channel without expired posts", live, 0, expiredOldest)
			backlog := f.seedTTLChannelLayout("TTL channel with unswept expired posts", live, expiredCountExpiredPosts, expiredOldest)
			cleanOffset := int64(live/2 + 2)
			backlogOffset := cleanOffset
			if expiredOldest {
				backlogOffset += expiredCountExpiredPosts
			}

			cleanStatements, _, cleanCount, cleanIDs := f.historyCostRequest(clean, cleanOffset, -5)
			cleanAroundRows := f.lastAroundRows
			assertAroundQuery("ChannelHistoryPageAround")
			backlogStatements, _, backlogCount, backlogIDs := f.historyCostRequest(backlog, backlogOffset, -5)
			backlogAroundRows := f.lastAroundRows
			wantBacklogQuery := "ChannelHistoryPageAround"
			if expiredCountExpiredPosts > live {
				wantBacklogQuery = "ChannelHistoryPageAroundUnexpired"
			}
			assertAroundQuery(wantBacklogQuery)
			bound := cleanAroundRows + int64(min(expiredCountExpiredPosts, live))
			t.Logf("live %d, expired oldest %v: around statement rows %d vs %d with %d expired unswept (bound %d)",
				live, expiredOldest, cleanAroundRows, backlogAroundRows, expiredCountExpiredPosts, bound)

			if expiredOldest {
				// The live posts follow the expired ones, so their IDs are shifted;
				// the service row at local ID 1 is not.
				for i, id := range backlogIDs {
					if id > 1 {
						backlogIDs[i] = id - expiredCountExpiredPosts
					}
				}
			}
			if !slices.Equal(backlogIDs, cleanIDs) {
				t.Fatalf("live %d, expired oldest %v: around page = %v, want the clean channel's page %v", live, expiredOldest, backlogIDs, cleanIDs)
			}
			if backlogCount != cleanCount {
				t.Fatalf("live %d, expired oldest %v: history counts with and without backlog = %d/%d, want equal", live, expiredOldest, backlogCount, cleanCount)
			}
			if backlogStatements != cleanStatements {
				t.Fatalf("live %d, expired oldest %v: statements with backlog = %d, no backlog = %d, want equal", live, expiredOldest, backlogStatements, cleanStatements)
			}
			if float64(backlogAroundRows) > 1.5*float64(bound) {
				t.Fatalf("live %d, expired oldest %v: around statement rows with backlog = %d, want within 1.5x of %d (no-backlog %d + min(backlog, live))",
					live, expiredOldest, backlogAroundRows, bound, cleanAroundRows)
			}
		}
	}
}
