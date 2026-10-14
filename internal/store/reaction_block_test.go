package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func reactionsAt(t *testing.T, s *store.Store, ownerID, localID int64) []store.Reaction {
	t.Helper()
	rs, err := s.ReactionsByOwnerLocal(context.Background(), ownerID, localID)
	if err != nil {
		t.Fatalf("reactions (%d,%d): %v", ownerID, localID, err)
	}
	return rs
}

func mustUnblock(t *testing.T, s *store.Store, blockerID, blockedID int64) {
	t.Helper()
	if _, err := s.UnblockUser(context.Background(), blockerID, blockedID); err != nil {
		t.Fatalf("unblock %d -> %d: %v", blockerID, blockedID, err)
	}
}

// TestDirectReactionsFromBlockedReactor covers both sides of a 1:1 message
// copy: a reactor the peer blocked writes and is answered for its own copy
// only, a reactor who blocked the peer is refused, clearing follows the same
// split except that the blocker may always clear, and unblocking delivers only
// later reactions.
func TestDirectReactionsFromBlockedReactor(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	for i, own := range []bool{true, false} {
		a, b := mustUser(t, s, fmt.Sprintf("+155518710%d1", i)), mustUser(t, s, fmt.Sprintf("+155518710%d2", i))
		// own: A reacts on the message A wrote; otherwise on B's message.
		var aLocal, bLocal int64
		if own {
			m := send(t, s, a, b, "hi", 1871001)
			aLocal, bLocal = m.LocalID, m.PeerLocalID
		} else {
			m := send(t, s, b, a, "hi", 1871002)
			aLocal, bLocal = m.PeerLocalID, m.LocalID
		}

		// A pre-block reaction reaches both copies.
		if _, err := s.SendReaction(ctx, a.ID, aLocal, "👍"); err != nil {
			t.Fatalf("own=%v pre-block react: %v", own, err)
		}
		mustBlock(t, s, b.ID, a.ID)

		// Set while the peer blocked the reactor: own copy only.
		targets, err := s.SendReaction(ctx, a.ID, aLocal, "❤")
		if err != nil {
			t.Fatalf("own=%v react while blocked: %v", own, err)
		}
		if want := []store.ReactionTarget{{OwnerID: a.ID, LocalID: aLocal}}; !reflect.DeepEqual(targets, want) {
			t.Fatalf("own=%v targets while blocked = %+v, want %+v", own, targets, want)
		}
		if got := reactionsAt(t, s, a.ID, aLocal); len(got) != 1 || got[0].Reaction != "❤" {
			t.Fatalf("own=%v reactor copy = %+v, want the new reaction", own, got)
		}
		if got := reactionsAt(t, s, b.ID, bLocal); len(got) != 1 || got[0].Reaction != "👍" {
			t.Fatalf("own=%v blocker copy = %+v, want the pre-block reaction only", own, got)
		}

		// Clear while the peer blocked the reactor: own copy only.
		targets, err = s.ClearReaction(ctx, a.ID, aLocal)
		if err != nil {
			t.Fatalf("own=%v clear while blocked: %v", own, err)
		}
		if want := []store.ReactionTarget{{OwnerID: a.ID, LocalID: aLocal}}; !reflect.DeepEqual(targets, want) {
			t.Fatalf("own=%v clear targets while blocked = %+v, want %+v", own, targets, want)
		}
		if got := reactionsAt(t, s, a.ID, aLocal); len(got) != 0 {
			t.Fatalf("own=%v reactor copy after clear = %+v, want none", own, got)
		}
		if got := reactionsAt(t, s, b.ID, bLocal); len(got) != 1 || got[0].Reaction != "👍" {
			t.Fatalf("own=%v blocker copy after clear = %+v, want the pre-block reaction kept", own, got)
		}

		// The blocker setting a reaction toward the blocked user is refused.
		if _, err := s.SendReaction(ctx, b.ID, bLocal, "🔥"); !errors.Is(err, store.ErrYouBlockedUser) {
			t.Fatalf("own=%v blocker react err = %v, want ErrYouBlockedUser", own, err)
		}
		if got := reactionsAt(t, s, a.ID, aLocal); len(got) != 0 {
			t.Fatalf("own=%v refused react wrote reactor copy: %+v", own, got)
		}
		if got := reactionsAt(t, s, b.ID, bLocal); len(got) != 1 || got[0].ReactorID != a.ID {
			t.Fatalf("own=%v refused react wrote blocker copy: %+v", own, got)
		}

		// Unblock: the withheld clear is not replayed, a later reaction flows.
		mustUnblock(t, s, b.ID, a.ID)
		if got := reactionsAt(t, s, b.ID, bLocal); len(got) != 1 || got[0].Reaction != "👍" {
			t.Fatalf("own=%v blocker copy after unblock = %+v, want unchanged", own, got)
		}
		targets, err = s.SendReaction(ctx, a.ID, aLocal, "🎉")
		if err != nil || len(targets) != 2 {
			t.Fatalf("own=%v react after unblock: targets=%+v err=%v", own, targets, err)
		}
		if got := reactionsAt(t, s, b.ID, bLocal); len(got) != 1 || got[0].Reaction != "🎉" {
			t.Fatalf("own=%v blocker copy after later react = %+v", own, got)
		}

		// The blocker clearing its own reaction reaches both copies.
		if _, err := s.SendReaction(ctx, b.ID, bLocal, "😀"); err != nil {
			t.Fatalf("own=%v unblocked B react: %v", own, err)
		}
		mustBlock(t, s, b.ID, a.ID)
		targets, err = s.ClearReaction(ctx, b.ID, bLocal)
		if err != nil || len(targets) != 2 {
			t.Fatalf("own=%v blocker clear: targets=%+v err=%v", own, targets, err)
		}
		for _, c := range []struct{ owner, local int64 }{{a.ID, aLocal}, {b.ID, bLocal}} {
			for _, r := range reactionsAt(t, s, c.owner, c.local) {
				if r.ReactorID == b.ID {
					t.Fatalf("own=%v blocker clear left %+v on owner %d", own, r, c.owner)
				}
			}
		}
	}
}

// TestDirectReactionBlockOrderedByPairLock commits a block while holding the
// pair lock a reaction takes, and proves the reaction waiting on that lock
// decides on the committed block rather than on a read taken before it.
func TestDirectReactionBlockOrderedByPairLock(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a, b := mustUser(t, s, "+15551871031"), mustUser(t, s, "+15551871032")
	m := send(t, s, a, b, "hi", 1871031)

	pool := store.StorePool(s)
	raw, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer raw.Release()
	tx, err := raw.Conn().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // deferred rollback cleans up this test-owned transaction
	ids := []int64{a.ID, b.ID}
	slices.Sort(ids)
	for _, id := range ids {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", id); err != nil {
			t.Fatalf("lock %d: %v", id, err)
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO blocked_users (blocker_id, blocked_id) VALUES ($1, $2)", b.ID, a.ID); err != nil {
		t.Fatalf("insert block: %v", err)
	}

	var targets []store.ReactionTarget
	var reactErr error
	done := make(chan struct{})
	go func() {
		targets, reactErr = s.SendReaction(ctx, a.ID, m.LocalID, "❤")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("reaction finished while the pair lock was held")
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	<-done
	if reactErr != nil {
		t.Fatalf("react: %v", reactErr)
	}
	if len(targets) != 1 || targets[0].OwnerID != a.ID {
		t.Fatalf("targets = %+v, want the reactor's copy only", targets)
	}
	if got := reactionsAt(t, s, b.ID, m.PeerLocalID); len(got) != 0 {
		t.Fatalf("blocker copy = %+v, want none", got)
	}
}
