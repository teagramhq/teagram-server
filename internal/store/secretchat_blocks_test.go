package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

func secretChatIDs(chats []store.SecretChat) []int32 {
	ids := make([]int32, len(chats))
	for i, c := range chats {
		ids[i] = c.ID
	}
	return ids
}

// assertSecretChatHidden fails when either difference read shows chatID to
// userID.
func assertSecretChatHidden(t *testing.T, s *store.Store, userID int64, chatID int32) {
	t.Helper()
	ctx := context.Background()
	after, err := s.SecretChatsAfterPosition(ctx, userID, time.Unix(0, 0), nil)
	if err != nil {
		t.Fatalf("after position: %v", err)
	}
	from, err := s.SecretChatsFromDate(ctx, userID, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("from date: %v", err)
	}
	for _, c := range append(after, from...) {
		if c.ID == chatID {
			t.Fatalf("user %d sees withheld chat %d: after=%v from=%v", userID, chatID, secretChatIDs(after), secretChatIDs(from))
		}
	}
}

func assertSecretChatShown(t *testing.T, s *store.Store, userID int64, chatID int32) {
	t.Helper()
	ctx := context.Background()
	after, err := s.SecretChatsAfterPosition(ctx, userID, time.Unix(0, 0), nil)
	if err != nil {
		t.Fatalf("after position: %v", err)
	}
	from, err := s.SecretChatsFromDate(ctx, userID, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("from date: %v", err)
	}
	for _, chats := range [][]store.SecretChat{after, from} {
		found := false
		for _, c := range chats {
			found = found || c.ID == chatID
		}
		if !found {
			t.Fatalf("user %d does not see chat %d: after=%v from=%v", userID, chatID, secretChatIDs(after), secretChatIDs(from))
		}
	}
}

// TestSecretChatRequestFromBlockedAdminIsWithheld: a request to a participant
// who blocked the admin is stored withheld, answers the admin as usual, is
// never shown to or acceptable by the participant, and stays withheld after an
// unblock and after the admin discards it.
func TestSecretChatRequestFromBlockedAdminIsWithheld(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	admin, err := s.CreateUser(ctx, "+15559987201")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	participant, err := s.CreateUser(ctx, "+15559987202")
	if err != nil {
		t.Fatalf("participant: %v", err)
	}
	if _, err := s.BlockUser(ctx, participant.ID, admin.ID); err != nil {
		t.Fatalf("block: %v", err)
	}

	ga := gaFor(1872)
	hash := sha256.Sum256(ga)
	const randomID = int64(18721)
	chat, dup, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], randomID, store.RateLimitConfig{})
	if err != nil {
		t.Fatalf("request while blocked: %v", err)
	}
	if dup || !chat.Withheld || chat.State != store.SecretChatRequested {
		t.Fatalf("request while blocked = %+v dup=%v, want a new withheld request", chat, dup)
	}
	assertSecretChatShown(t, s, admin.ID, chat.ID)
	assertSecretChatHidden(t, s, participant.ID, chat.ID)
	if _, err := s.AcceptSecretChat(ctx, chat.ID, participant.ID, gaFor(2), 7); !errors.Is(err, store.ErrSecretChatStale) {
		t.Fatalf("accept withheld = %v, want ErrSecretChatStale", err)
	}

	if _, err := s.UnblockUser(ctx, participant.ID, admin.ID); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	resent, dup, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], randomID, store.RateLimitConfig{})
	if err != nil {
		t.Fatalf("resend after unblock: %v", err)
	}
	if !dup || resent.ID != chat.ID || !resent.Withheld {
		t.Fatalf("resend after unblock = %+v dup=%v, want the withheld chat %d", resent, dup, chat.ID)
	}
	assertSecretChatHidden(t, s, participant.ID, chat.ID)

	discarded, err := s.DiscardSecretChat(ctx, chat.ID, admin.ID, store.RateLimitConfig{})
	if err != nil {
		t.Fatalf("admin discard: %v", err)
	}
	if !discarded.Withheld {
		t.Fatalf("discarded chat lost its withheld flag: %+v", discarded)
	}
	assertSecretChatShown(t, s, admin.ID, chat.ID)
	assertSecretChatHidden(t, s, participant.ID, chat.ID)

	later, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], randomID+1, store.RateLimitConfig{})
	if err != nil {
		t.Fatalf("request after unblock: %v", err)
	}
	if later.Withheld {
		t.Fatalf("request after unblock withheld: %+v", later)
	}
	assertSecretChatShown(t, s, participant.ID, later.ID)
}

// TestSecretChatRequestWithheldKeepsCapAndBudget: a withheld request counts
// against the admin's outstanding cap and budget exactly as a delivered one.
func TestSecretChatRequestWithheldKeepsCapAndBudget(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	admin, err := s.CreateUser(ctx, "+15559987211")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	participant, err := s.CreateUser(ctx, "+15559987212")
	if err != nil {
		t.Fatalf("participant: %v", err)
	}
	if _, err := s.BlockUser(ctx, participant.ID, admin.ID); err != nil {
		t.Fatalf("block: %v", err)
	}

	budget := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	ga := gaFor(1)
	hash := sha256.Sum256(ga)
	if _, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], 1, budget); err != nil {
		t.Fatalf("first withheld request: %v", err)
	}
	_, _, err = s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], 2, budget)
	if flooded, ok := errors.AsType[*store.RateLimitedError](err); !ok || flooded == nil {
		t.Fatalf("second withheld request over budget = %v, want RateLimitedError", err)
	}

	for i := 1; i < store.MaxOutstandingSecretChats; i++ {
		if _, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], int64(100+i), store.RateLimitConfig{}); err != nil {
			t.Fatalf("withheld request %d: %v", i, err)
		}
	}
	if _, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], 999, store.RateLimitConfig{}); !errors.Is(err, store.ErrSecretChatsTooMany) {
		t.Fatalf("withheld request past the cap = %v, want ErrSecretChatsTooMany", err)
	}
}

// TestSecretChatRequestToBlockedUserIsRefused: an admin who blocked the
// participant gets ErrYouBlockedUser with no row, no id consumed and no budget
// charged.
func TestSecretChatRequestToBlockedUserIsRefused(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	admin, err := s.CreateUser(ctx, "+15559987221")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	participant, err := s.CreateUser(ctx, "+15559987222")
	if err != nil {
		t.Fatalf("participant: %v", err)
	}
	ga := gaFor(3)
	hash := sha256.Sum256(ga)
	first, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], 0, store.RateLimitConfig{})
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	budget := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	for _, mutual := range []bool{false, true} {
		if _, err := s.BlockUser(ctx, admin.ID, participant.ID); err != nil {
			t.Fatalf("block: %v", err)
		}
		if mutual {
			if _, err := s.BlockUser(ctx, participant.ID, admin.ID); err != nil {
				t.Fatalf("mutual block: %v", err)
			}
		}
		if _, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], 5, budget); !errors.Is(err, store.ErrYouBlockedUser) {
			t.Fatalf("request to blocked user (mutual=%v) = %v, want ErrYouBlockedUser", mutual, err)
		}
	}
	after, err := s.SecretChatsAfterPosition(ctx, admin.ID, time.Unix(0, 0), nil)
	if err != nil {
		t.Fatalf("after position: %v", err)
	}
	if len(after) != 1 || after[0].ID != first.ID {
		t.Fatalf("admin chats = %v, want only %d", secretChatIDs(after), first.ID)
	}

	for _, pair := range [][2]int64{{admin.ID, participant.ID}, {participant.ID, admin.ID}} {
		if _, err := s.UnblockUser(ctx, pair[0], pair[1]); err != nil {
			t.Fatalf("unblock: %v", err)
		}
	}
	next, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], 5, budget)
	if err != nil {
		t.Fatalf("request after unblock = %v, want the unspent budget", err)
	}
	if next.ID != first.ID+1 || next.Withheld {
		t.Fatalf("request after unblock = %+v, want id %d delivered", next, first.ID+1)
	}
}

// TestBlockKeepsActiveSecretChat: blocking does not discard a chat that is
// already active.
func TestBlockKeepsActiveSecretChat(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	admin, err := s.CreateUser(ctx, "+15559987231")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	participant, err := s.CreateUser(ctx, "+15559987232")
	if err != nil {
		t.Fatalf("participant: %v", err)
	}
	ga := gaFor(4)
	hash := sha256.Sum256(ga)
	chat, _, err := s.CreateSecretChatRequest(ctx, admin.ID, participant.ID, ga, hash[:], 0, store.RateLimitConfig{})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := s.AcceptSecretChat(ctx, chat.ID, participant.ID, gaFor(5), 9); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := s.BlockUser(ctx, participant.ID, admin.ID); err != nil {
		t.Fatalf("block: %v", err)
	}
	got, err := s.SecretChatByID(ctx, chat.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.State != store.SecretChatActive || got.Withheld {
		t.Fatalf("chat after block = %+v, want active and not withheld", got)
	}
	assertSecretChatShown(t, s, participant.ID, chat.ID)
}
