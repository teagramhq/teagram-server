package store

import (
	"context"
	"testing"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestMessagesForLookupSnapshotKeepsEstablishedMembershipView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := Open(ctx, dsn, pgtest.EncKey(), WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	creator, err := s.CreateUser(ctx, "+15551291401")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551291402")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "snapshot membership", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message, _, _, err := s.SendChatMessage(ctx, FanOut{ChatID: chat.ID, FromID: creator.ID, Text: "snapshot body", RandomID: 91401})
	if err != nil {
		t.Fatalf("send chat message: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	s.getMessagesSnapshotHook = func() {
		close(started)
		<-release
	}
	type outcome struct {
		snapshot MessagesReadSnapshot
		err      error
	}
	result := make(chan outcome, 1)
	go func() {
		snapshot, readErr := s.MessagesForLookupSnapshot(ctx, member.ID, []MessageLookup{{ID: 1}})
		result <- outcome{snapshot: snapshot, err: readErr}
	}()
	<-started
	if _, _, _, err := s.RemoveChatUser(ctx, chat.ID, member.ID, creator.ID); err != nil {
		close(release)
		t.Fatalf("commit member removal: %v", err)
	}
	close(release)
	read := <-result
	s.getMessagesSnapshotHook = nil
	if read.err != nil {
		t.Fatalf("read established snapshot: %v", read.err)
	}
	if len(read.snapshot.Results) != 1 || read.snapshot.Results[0].Message == nil || read.snapshot.Results[0].Message.LocalID != message.LocalID {
		t.Fatalf("established snapshot result = %+v, want retained row %d", read.snapshot.Results, message.LocalID)
	}
	if got := read.snapshot.ChatParticipantCounts[chat.ID]; got != 2 {
		t.Fatalf("established snapshot participant count = %d, want old count 2", got)
	}
	if _, ok := read.snapshot.Chats[chat.ID]; !ok {
		t.Fatal("established snapshot omitted chat metadata for current member")
	}

	afterRemoval, err := s.MessagesForLookupSnapshot(ctx, member.ID, []MessageLookup{{ID: 1}})
	if err != nil {
		t.Fatalf("read after committed removal: %v", err)
	}
	if len(afterRemoval.Results) != 1 || afterRemoval.Results[0].Message != nil || len(afterRemoval.Users) != 0 || len(afterRemoval.Chats) != 0 || len(afterRemoval.Files) != 0 {
		t.Fatalf("subsequent snapshot disclosed removed row or hydration: %+v", afterRemoval)
	}
}
