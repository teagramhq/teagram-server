//nolint:testpackage // This test verifies transactional hooks and database constraints.
package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func openDialogPinsStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}
	s, err := Open(ctx, pgtest.DSN(t), pgtest.EncKey(), WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func TestDialogPinPositionMustBePresent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openDialogPinsStore(t)
	owner, err := s.CreateUser(ctx, "+15551360001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_dialog_pins (owner_id, peer_type, peer_id, position)
		VALUES ($1, 1, 999, NULL)
	`, owner.ID); err == nil {
		t.Fatal("pin without a position passed the schema constraint")
	}
}

func TestChannelRemovalSerializesWithDialogPinMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openDialogPinsStore(t)
	creator, err := s.CreateUser(ctx, "+15551360002")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	owner, err := s.CreateUser(ctx, "+15551360003")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Pin race", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create channel invite: %v", err)
	}
	if _, _, err := s.JoinChannelByInvite(ctx, invite, owner.ID); err != nil {
		t.Fatalf("join channel: %v", err)
	}

	pinLocked := make(chan struct{})
	releasePin := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePin) }) }
	t.Cleanup(release)
	s.dialogPinMutationHook = func() {
		close(pinLocked)
		<-releasePin
	}
	leaveAtOwnerLock := make(chan struct{})
	s.leaveChannelOwnerLockHook = func() { close(leaveAtOwnerLock) }

	type pinResult struct {
		changed bool
		err     error
	}
	toggleDone := make(chan pinResult, 1)
	go func() {
		changed, err := s.ToggleDialogPin(ctx, owner.ID, DialogPinPeer{PeerType: PeerTypeChannel, PeerID: channel.ID}, true, time.Now())
		toggleDone <- pinResult{changed: changed, err: err}
	}()
	<-pinLocked

	type leaveResult struct {
		left bool
		err  error
	}
	leaveDone := make(chan leaveResult, 1)
	go func() {
		left, err := s.LeaveChannel(ctx, channel.ID, owner.ID)
		leaveDone <- leaveResult{left: left, err: err}
	}()
	<-leaveAtOwnerLock
	select {
	case result := <-leaveDone:
		t.Fatalf("channel removal passed the owner's pin lock: left=%v err=%v", result.left, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	s.dialogPinMutationHook = nil
	s.leaveChannelOwnerLockHook = nil
	if result := <-toggleDone; result.err != nil || !result.changed {
		t.Fatalf("pin that acquired the owner lock first: changed=%v err=%v", result.changed, result.err)
	}
	if result := <-leaveDone; result.err != nil || !result.left {
		t.Fatalf("removal after pin: left=%v err=%v", result.left, result.err)
	}

	snapshot, err := s.DialogPinsPeerSnapshot(ctx, owner.ID, time.Now())
	if err != nil {
		t.Fatalf("read pins after channel removal: %v", err)
	}
	if len(snapshot.Pins) != 0 || len(snapshot.PeerDialogs.Dialogs) != 0 {
		t.Fatalf("removed channel remained visible: pins=%+v dialogs=%+v", snapshot.Pins, snapshot.PeerDialogs.Dialogs)
	}
	if _, err := s.ToggleDialogPin(ctx, owner.ID, DialogPinPeer{PeerType: PeerTypeChannel, PeerID: channel.ID}, true, time.Now()); !errors.Is(err, ErrDialogPinPeerInvalid) {
		t.Fatalf("pin after committed removal error = %v, want inaccessible peer", err)
	}
}
