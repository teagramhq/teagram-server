package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestMarkDialogUnreadTimestampFollowsOwnerLockWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15551399101")
	peer := mustUser(t, s, "+15551399102")
	send(t, s, peer, owner, "seed dialog", 991901)

	releaseOwnerLock, err := store.HoldOwnerLock(ctx, s, owner.ID)
	if err != nil {
		t.Fatalf("hold owner lock: %v", err)
	}
	defer releaseOwnerLock()

	type mutationResult struct {
		changed bool
		err     error
	}
	mutationDone := make(chan mutationResult, 1)
	mutationCtx, cancelMutation := context.WithTimeout(ctx, 10*time.Second)
	defer cancelMutation()
	go func() {
		changed, err := s.MarkDialogUnread(mutationCtx, owner.ID, store.PeerDialogKey{
			PeerType: store.PeerTypeUser,
			PeerID:   peer.ID,
		}, true)
		mutationDone <- mutationResult{changed: changed, err: err}
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	if err := store.WaitForLockWaiters(waitCtx, s, 1); err != nil {
		cancelWait()
		t.Fatalf("unread mark mutation did not wait on the owner lock: %v", err)
	}
	cancelWait()
	// The transaction is already waiting, so this crosses at least one
	// wall-clock second after its transaction-start timestamp was captured.
	time.Sleep(1100 * time.Millisecond)
	lockReleaseAt := time.Now()
	releaseOwnerLock()

	select {
	case result := <-mutationDone:
		if result.err != nil || !result.changed {
			t.Fatalf("mark dialog unread after owner lock release: changed=%v err=%v", result.changed, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unread mark mutation remained blocked after owner lock release")
	}

	mark, found, err := s.DialogUnreadMarkStateForPeer(ctx, owner.ID, store.PeerDialogKey{
		PeerType: store.PeerTypeUser,
		PeerID:   peer.ID,
	})
	if err != nil || !found {
		t.Fatalf("read saved unread mark: found=%v err=%v", found, err)
	}
	if mark.ChangedAt.Before(lockReleaseAt) {
		t.Fatalf("changed_at = %s, earlier than owner lock release %s", mark.ChangedAt, lockReleaseAt)
	}
}
