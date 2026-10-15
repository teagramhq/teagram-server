package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

// activeSecretChat opens an accepted chat from admin to participant.
func activeSecretChat(t *testing.T, s *store.Store, admin, participant int64) store.SecretChat {
	t.Helper()
	ctx := context.Background()
	ga := gaFor(18)
	gaHash := sha256.Sum256(ga)
	chat, _, err := s.CreateSecretChatRequest(ctx, admin, participant, ga, gaHash[:], 0, store.RateLimitConfig{})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	accepted, err := s.AcceptSecretChat(ctx, chat.ID, participant, gaFor(19), 1873)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	return accepted
}

// assertNoEncryptedEvents fails when ownerID has any stored encrypted event or
// a qts other than wantQts.
func assertNoEncryptedEvents(t *testing.T, s *store.Store, ownerID int64, wantQts int) {
	t.Helper()
	ctx := context.Background()
	state, err := s.State(ctx, ownerID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.Qts != wantQts {
		t.Fatalf("qts = %d, want %d", state.Qts, wantQts)
	}
	events, err := s.EncryptedEventsWindow(ctx, ownerID, 0, 1<<30, 10)
	if err != nil {
		t.Fatalf("events window: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("recipient has %d encrypted events, want none", len(events))
	}
}

// TestSendEncryptedMessageToBlockerIsWithheld: a message to a recipient who
// blocked the sender answers with a date and stores nothing for the recipient;
// a resend after the unblock returns the original date and still delivers
// nothing, while a new message after the unblock is delivered.
func TestSendEncryptedMessageToBlockerIsWithheld(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559987301")
	b := mustUser(t, s, "+15559987302")
	chat := activeSecretChat(t, s, a.ID, b.ID)

	if _, err := s.BlockUser(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("block: %v", err)
	}
	const withheldRandomID = 18730001
	event, dup, err := s.SendEncryptedMessage(ctx, store.EncryptedSend{
		SenderID: a.ID, RecipientID: b.ID, ChatID: chat.ID, RandomID: withheldRandomID, Data: []byte("withheld"),
	})
	if err != nil {
		t.Fatalf("send while blocked: %v", err)
	}
	if dup || !event.Withheld || event.Date.IsZero() {
		t.Fatalf("send while blocked = %+v dup=%v, want a new withheld answer with a date", event, dup)
	}
	assertNoEncryptedEvents(t, s, b.ID, 0)

	pre, ok, err := s.EncryptedMessageByRandomID(ctx, b.ID, withheldRandomID)
	if err != nil || !ok || pre.Withheld || !pre.Date.Equal(event.Date) {
		t.Fatalf("pre-check lookup = %+v ok=%v err=%v, want the original answer at %v", pre, ok, err, event.Date)
	}

	if _, err := s.UnblockUser(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	resent, dup, err := s.SendEncryptedMessage(ctx, store.EncryptedSend{
		SenderID: a.ID, RecipientID: b.ID, ChatID: chat.ID, RandomID: withheldRandomID, Data: []byte("withheld"),
	})
	if err != nil {
		t.Fatalf("resend after unblock: %v", err)
	}
	if !dup || resent.Withheld || !resent.Date.Equal(event.Date) {
		t.Fatalf("resend after unblock = %+v dup=%v, want the original answer at %v", resent, dup, event.Date)
	}
	assertNoEncryptedEvents(t, s, b.ID, 0)

	delivered, dup, err := s.SendEncryptedMessage(ctx, store.EncryptedSend{
		SenderID: a.ID, RecipientID: b.ID, ChatID: chat.ID, RandomID: 18730002, Data: []byte("delivered"),
	})
	if err != nil {
		t.Fatalf("send after unblock: %v", err)
	}
	if dup || delivered.Withheld || delivered.Qts != 1 {
		t.Fatalf("send after unblock = %+v dup=%v, want a delivered event at qts 1", delivered, dup)
	}
}

// TestSendEncryptedMessageToBlockedUserRefused: a sender who blocked the
// recipient gets ErrYouBlockedUser, including under a mutual block, and
// nothing is recorded, so the same random_id is delivered after the unblock.
func TestSendEncryptedMessageToBlockedUserRefused(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559987311")
	b := mustUser(t, s, "+15559987312")
	chat := activeSecretChat(t, s, a.ID, b.ID)
	send := func() (store.EncryptedEvent, bool, error) {
		return s.SendEncryptedMessage(ctx, store.EncryptedSend{
			SenderID: a.ID, RecipientID: b.ID, ChatID: chat.ID, RandomID: 18730011, Data: []byte("refused"),
		})
	}

	if _, err := s.BlockUser(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("block: %v", err)
	}
	for _, mutual := range []bool{false, true} {
		if mutual {
			if _, err := s.BlockUser(ctx, b.ID, a.ID); err != nil {
				t.Fatalf("mutual block: %v", err)
			}
		}
		if _, _, err := send(); !errors.Is(err, store.ErrYouBlockedUser) {
			t.Fatalf("send (mutual=%v) err = %v, want ErrYouBlockedUser", mutual, err)
		}
		if _, ok, err := s.EncryptedMessageByRandomID(ctx, b.ID, 18730011); err != nil || ok {
			t.Fatalf("refused send (mutual=%v) recorded an answer: ok=%v err=%v", mutual, ok, err)
		}
		assertNoEncryptedEvents(t, s, b.ID, 0)
	}

	for _, pair := range [][2]int64{{a.ID, b.ID}, {b.ID, a.ID}} {
		if _, err := s.UnblockUser(ctx, pair[0], pair[1]); err != nil {
			t.Fatalf("unblock: %v", err)
		}
	}
	event, dup, err := send()
	if err != nil || dup || event.Withheld || event.Qts != 1 {
		t.Fatalf("send after unblock = %+v dup=%v err=%v, want delivered at qts 1", event, dup, err)
	}
}

// TestSendEncryptedMessageOrderedAgainstBlock holds the sender's owner lock,
// which BlockUser takes, commits a block under it, and only then lets a send
// through: the send must wait on that lock and see the block.
func TestSendEncryptedMessageOrderedAgainstBlock(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559987321")
	b := mustUser(t, s, "+15559987322")
	chat := activeSecretChat(t, s, a.ID, b.ID)

	raw, err := store.StorePool(s).Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer raw.Release()
	tx, err := raw.Conn().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // deferred rollback cleans up this test-owned transaction

	// Only the sender's lock: the recipient lock alone, the old leaf lock,
	// would let the send through before the block commits.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", a.ID); err != nil {
		t.Fatalf("lock sender: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO blocked_users (blocker_id, blocked_id) VALUES ($1, $2)", b.ID, a.ID); err != nil {
		t.Fatalf("block under lock: %v", err)
	}

	var (
		event   store.EncryptedEvent
		sendErr error
	)
	done := make(chan struct{})
	go func() {
		event, _, sendErr = s.SendEncryptedMessage(ctx, store.EncryptedSend{
			SenderID: a.ID, RecipientID: b.ID, ChatID: chat.ID, RandomID: 18730021, Data: []byte("raced"),
		})
		close(done)
	}()
	if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
		t.Fatalf("send never waited on the sender lock: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send did not return after the block committed")
	}
	if sendErr != nil || !event.Withheld {
		t.Fatalf("send racing a block = %+v err=%v, want withheld", event, sendErr)
	}
	assertNoEncryptedEvents(t, s, b.ID, 0)
}

// TestSendEncryptedMessageResendAfterAckKeepsAnswer: a delivered message the
// recipient has acked answers a resend exactly as a withheld one does, with
// the original date and nothing delivered again, so the answer cannot tell
// the sender whether the recipient received it or blocked them.
func TestSendEncryptedMessageResendAfterAckKeepsAnswer(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559987331")
	b := mustUser(t, s, "+15559987332")
	chat := activeSecretChat(t, s, a.ID, b.ID)
	send := func() (store.EncryptedEvent, bool) {
		event, dup, err := s.SendEncryptedMessage(ctx, store.EncryptedSend{
			SenderID: a.ID, RecipientID: b.ID, ChatID: chat.ID, RandomID: 18730031, Data: []byte("acked"),
		})
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		return event, dup
	}

	first, dup := send()
	if dup || first.Withheld || first.Qts != 1 {
		t.Fatalf("first send = %+v dup=%v, want delivered at qts 1", first, dup)
	}
	acked, err := s.AcknowledgeEncryptedEvents(ctx, b.ID, int64(first.Qts))
	if err != nil || len(acked) != 1 {
		t.Fatalf("ack = %v, %v; want the one event", acked, err)
	}

	pre, ok, err := s.EncryptedMessageByRandomID(ctx, b.ID, 18730031)
	if err != nil || !ok || !pre.Date.Equal(first.Date) {
		t.Fatalf("pre-check after ack = %+v ok=%v err=%v, want the original date %v", pre, ok, err, first.Date)
	}
	resent, dup := send()
	if !dup || resent.Withheld || !resent.Date.Equal(first.Date) {
		t.Fatalf("resend after ack = %+v dup=%v, want the original answer at %v", resent, dup, first.Date)
	}
	assertNoEncryptedEvents(t, s, b.ID, 1)
}
