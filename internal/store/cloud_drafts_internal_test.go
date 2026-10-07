package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

func openCloudDraftStore(t *testing.T) *Store {
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

func TestSaveCloudDraftWaitsForConcurrentChatRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCloudDraftStore(t)
	creator, err := s.CreateUser(ctx, "+15551368001")
	if err != nil {
		t.Fatalf("create chat creator: %v", err)
	}
	owner, err := s.CreateUser(ctx, "+15551368002")
	if err != nil {
		t.Fatalf("create chat member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "draft removal", []int64{owner.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}

	removal, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin removal transaction: %v", err)
	}
	qtx := s.q.WithTx(removal)
	if _, err = qtx.ChatByIDForUpdate(ctx, chat.ID); err != nil {
		t.Fatalf("lock chat for removal: %v", err)
	}
	if _, err = qtx.DeleteChatParticipant(ctx, db.DeleteChatParticipantParams{ChatID: chat.ID, UserID: owner.ID}); err != nil {
		t.Fatalf("remove chat participant: %v", err)
	}
	saveResult := make(chan error, 1)
	go func() {
		_, _, saveErr := s.SaveCloudDraft(ctx, owner.ID, PeerTypeChat, chat.ID, "private", false, 0)
		saveResult <- saveErr
	}()
	select {
	case saveErr := <-saveResult:
		if rollbackErr := removal.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback participant removal: %v", rollbackErr)
		}
		t.Fatalf("draft save finished before removal committed: %v", saveErr)
	case <-time.After(50 * time.Millisecond):
	}
	if err = removal.Commit(ctx); err != nil {
		t.Fatalf("commit participant removal: %v", err)
	}
	select {
	case saveErr := <-saveResult:
		if !errors.Is(saveErr, ErrNotMember) {
			t.Fatalf("draft save after committed removal = %v, want ErrNotMember", saveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("draft save remained blocked after removal committed")
	}
}

func TestSaveCloudDraftWaitsForConcurrentChannelRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openCloudDraftStore(t)
	creator, err := s.CreateUser(ctx, "+15551368003")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	owner, err := s.CreateUser(ctx, "+15551368004")
	if err != nil {
		t.Fatalf("create channel member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "draft removal", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(ctx, invite, owner.ID); err != nil {
		t.Fatalf("join channel: %v", err)
	}

	removal, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin removal transaction: %v", err)
	}
	qtx := s.q.WithTx(removal)
	if _, err = qtx.LockChannel(ctx, channel.ID); err != nil {
		t.Fatalf("lock channel for removal: %v", err)
	}
	if _, err = qtx.DeleteChannelParticipant(ctx, db.DeleteChannelParticipantParams{ChannelID: channel.ID, UserID: owner.ID}); err != nil {
		t.Fatalf("remove channel participant: %v", err)
	}
	saveResult := make(chan error, 1)
	go func() {
		_, _, saveErr := s.SaveCloudDraft(ctx, owner.ID, PeerTypeChannel, channel.ID, "private", false, 0)
		saveResult <- saveErr
	}()
	select {
	case saveErr := <-saveResult:
		if rollbackErr := removal.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback participant removal: %v", rollbackErr)
		}
		t.Fatalf("draft save finished before removal committed: %v", saveErr)
	case <-time.After(50 * time.Millisecond):
	}
	if err = removal.Commit(ctx); err != nil {
		t.Fatalf("commit participant removal: %v", err)
	}
	select {
	case saveErr := <-saveResult:
		if !errors.Is(saveErr, ErrNotMember) {
			t.Fatalf("draft save after committed removal = %v, want ErrNotMember", saveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("draft save remained blocked after removal committed")
	}
}
