package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSweepExpiredMessageCopiesDeletesEachOwnerCopyAndPreservesUnstamped(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	alice := mustUser(t, s, "+15551249901")
	bob := mustUser(t, s, "+15551249902")
	expiring := send(t, s, alice, bob, "expires", 2245001)
	untouched := send(t, s, alice, bob, "no TTL", 2245002)

	if _, err := store.StorePool(s).Exec(ctx, `UPDATE messages
		SET expires_at = clock_timestamp() - interval '1 second'
		WHERE (owner_id = $1 AND local_id = $2) OR (owner_id = $3 AND local_id = $4)`,
		alice.ID, expiring.LocalID, bob.ID, expiring.PeerLocalID); err != nil {
		t.Fatalf("expire message copies: %v", err)
	}

	deleted, more, err := s.SweepExpiredMessageCopies(ctx, 100)
	if err != nil {
		t.Fatalf("sweep expired message copies: %v", err)
	}
	if deleted != 2 || more {
		t.Fatalf("sweep result = deleted %d, more %t, want 2, false", deleted, more)
	}

	for _, owner := range []struct {
		id      int64
		localID int64
		keepID  int64
	}{
		{id: alice.ID, localID: expiring.LocalID, keepID: untouched.LocalID},
		{id: bob.ID, localID: expiring.PeerLocalID, keepID: untouched.PeerLocalID},
	} {
		var expiredDeleted, unstampedDeleted bool
		if err = store.StorePool(s).QueryRow(ctx, `SELECT deleted FROM messages WHERE owner_id = $1 AND local_id = $2`, owner.id, owner.localID).Scan(&expiredDeleted); err != nil {
			t.Fatalf("read expired copy %d:%d: %v", owner.id, owner.localID, err)
		}
		if err = store.StorePool(s).QueryRow(ctx, `SELECT deleted FROM messages WHERE owner_id = $1 AND local_id = $2`, owner.id, owner.keepID).Scan(&unstampedDeleted); err != nil {
			t.Fatalf("read unstamped copy %d:%d: %v", owner.id, owner.keepID, err)
		}
		if !expiredDeleted {
			t.Fatalf("expired copy %d:%d is not deleted", owner.id, owner.localID)
		}
		if unstampedDeleted {
			t.Fatalf("unstamped copy %d:%d was deleted", owner.id, owner.keepID)
		}
		events, eventErr := s.EventsSince(ctx, owner.id, 0)
		if eventErr != nil {
			t.Fatalf("load events for owner %d: %v", owner.id, eventErr)
		}
		var deleteCount int
		for _, event := range events {
			if event.Type == store.EventDelete {
				deleteCount++
				if event.LocalID != owner.localID {
					t.Errorf("owner %d delete event local ID = %d, want %d", owner.id, event.LocalID, owner.localID)
				}
			}
		}
		if deleteCount != 1 {
			t.Errorf("owner %d has %d delete events, want 1", owner.id, deleteCount)
		}
	}
}

func TestConcurrentExpiredMessageSweepsDeleteCopiesOnce(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	alice := mustUser(t, s, "+15551249911")
	bob := mustUser(t, s, "+15551249912")
	message := send(t, s, alice, bob, "expire once", 2245011)
	if _, err := store.StorePool(s).Exec(ctx, `UPDATE messages
		SET expires_at = clock_timestamp() - interval '1 second'
		WHERE (owner_id = $1 AND local_id = $2) OR (owner_id = $3 AND local_id = $4)`,
		alice.ID, message.LocalID, bob.ID, message.PeerLocalID); err != nil {
		t.Fatalf("expire message copies: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	deleted := make(chan int, 2)
	errCh := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			<-start
			count, _, sweepErr := s.SweepExpiredMessageCopies(ctx, 100)
			if sweepErr != nil {
				errCh <- sweepErr
				return
			}
			deleted <- count
		})
	}
	close(start)
	wg.Wait()
	close(deleted)
	close(errCh)
	for sweepErr := range errCh {
		t.Fatalf("concurrent sweep: %v", sweepErr)
	}
	var total int
	for count := range deleted {
		total += count
	}
	if total != 2 {
		t.Fatalf("concurrent sweeps deleted %d copies, want 2", total)
	}
	for _, ownerID := range []int64{alice.ID, bob.ID} {
		events, err := s.EventsSince(ctx, ownerID, 0)
		if err != nil {
			t.Fatalf("load events for owner %d: %v", ownerID, err)
		}
		var deletes int
		for _, event := range events {
			if event.Type == store.EventDelete {
				deletes++
			}
		}
		if deletes != 1 {
			t.Errorf("owner %d has %d delete events after concurrent sweep, want 1", ownerID, deletes)
		}
	}
}

func TestExpiredMessageSweepResumesAfterStoreRestart(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	user := mustUser(t, s, "+15551249921")
	first := send(t, s, user, user, "first", 2245021)
	second := send(t, s, user, user, "second", 2245022)
	if _, err := store.StorePool(s).Exec(ctx, `UPDATE messages
		SET expires_at = clock_timestamp() - interval '1 second'
		WHERE owner_id = $1 AND local_id = ANY($2::bigint[])`, user.ID, []int64{first.LocalID, second.LocalID}); err != nil {
		t.Fatalf("expire self-message copies: %v", err)
	}

	deleted, more, err := s.SweepExpiredMessageCopies(ctx, 1)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if deleted != 1 || !more {
		t.Fatalf("first sweep = deleted %d, more %t, want 1, true", deleted, more)
	}
	if err = s.Close(); err != nil {
		t.Fatalf("close store before restart: %v", err)
	}

	restarted := openStore(t, dsn)
	deleted, more, err = restarted.SweepExpiredMessageCopies(ctx, 1)
	if err != nil {
		t.Fatalf("sweep after restart: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("post-restart sweep deleted %d copies, want 1", deleted)
	}
	if more {
		deleted, more, err = restarted.SweepExpiredMessageCopies(ctx, 1)
		if err != nil {
			t.Fatalf("drain after restart: %v", err)
		}
		if deleted != 0 || more {
			t.Fatalf("final sweep = deleted %d, more %t, want 0, false", deleted, more)
		}
	}

	for _, localID := range []int64{first.LocalID, second.LocalID} {
		var deleted bool
		if err = store.StorePool(restarted).QueryRow(ctx, `SELECT deleted FROM messages WHERE owner_id = $1 AND local_id = $2`, user.ID, localID).Scan(&deleted); err != nil {
			t.Errorf("read expired self-message %d after restart sweep: %v", localID, err)
		} else if !deleted {
			t.Errorf("expired self-message %d survived restart sweep", localID)
		}
	}
}

func TestSweepExpiredChannelPostsEmitsDeleteEvent(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15551249931")
	channel := mustChannel(t, s, creator.ID, "TTL sweep channel")
	post := postAs(t, s, channel.ID, creator.ID, "expired post", 2245031)
	before, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before sweep: %v", err)
	}
	if _, err = store.StorePool(s).Exec(ctx, `UPDATE channel_messages
		SET expires_at = clock_timestamp() - interval '1 second'
		WHERE channel_id = $1 AND local_id = $2`, channel.ID, post.LocalID); err != nil {
		t.Fatalf("expire channel post: %v", err)
	}

	deleted, more, err := s.SweepExpiredChannelPosts(ctx, 100)
	if err != nil {
		t.Fatalf("sweep expired channel posts: %v", err)
	}
	if deleted != 1 || more {
		t.Fatalf("channel sweep = deleted %d, more %t, want 1, false", deleted, more)
	}
	after, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state after sweep: %v", err)
	}
	if after != before+1 {
		t.Fatalf("channel pts moved %d -> %d, want one delete bump", before, after)
	}
	events, err := s.ChannelEventsWindow(ctx, channel.ID, before, after, 10)
	if err != nil {
		t.Fatalf("channel events after sweep: %v", err)
	}
	if len(events) != 1 || events[0].Type != store.EventDelete || events[0].LocalID != post.LocalID || events[0].Pts != after {
		t.Fatalf("channel events after sweep = %+v, want one delete event for post %d at pts %d", events, post.LocalID, after)
	}
	var isDeleted bool
	if err = store.StorePool(s).QueryRow(ctx, `SELECT deleted FROM channel_messages WHERE channel_id = $1 AND local_id = $2`, channel.ID, post.LocalID).Scan(&isDeleted); err != nil {
		t.Fatalf("read expired channel post tombstone: %v", err)
	}
	if !isDeleted {
		t.Fatalf("expired channel post %d is not tombstoned", post.LocalID)
	}
}
