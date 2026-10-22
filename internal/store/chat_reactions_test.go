package store_test

import (
	"context"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSetChatAvailableReactionsPersistsForMemberSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator, err := s.CreateUser(ctx, "+15551348601")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Reactions", nil)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}

	for _, want := range []store.ChatAvailableReactions{
		{Mode: store.ChatAvailableReactionsNone},
		{Mode: store.ChatAvailableReactionsSome, Emoticons: []string{"❤"}},
		{Mode: store.ChatAvailableReactionsAll},
	} {
		if err := s.SetChatAvailableReactions(ctx, chat.ID, creator.ID, want); err != nil {
			t.Fatalf("set reactions %q: %v", want.Mode, err)
		}
		snapshot, err := s.ChatInfoForMemberSnapshot(ctx, creator.ID, []int64{chat.ID})
		if err != nil {
			t.Fatalf("read chat reaction snapshot: %v", err)
		}
		got, ok := snapshot.AvailableReactions[chat.ID]
		if !ok {
			t.Fatalf("snapshot omitted stored %q setting", want.Mode)
		}
		if got.Mode != want.Mode || len(got.Emoticons) != len(want.Emoticons) {
			t.Fatalf("stored reactions = %+v, want %+v", got, want)
		}
		for i := range want.Emoticons {
			if got.Emoticons[i] != want.Emoticons[i] {
				t.Fatalf("stored reactions = %+v, want %+v", got, want)
			}
		}
	}
}
