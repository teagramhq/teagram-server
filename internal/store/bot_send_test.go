package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSendBotMessageRequiresStartedHumanAndUsesTwoSidedPath(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	bot, _, err := s.CreateBot(ctx, "send_test_bot", "Send test")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	otherBot, _, err := s.CreateBot(ctx, "other_send_test_bot", "Other")
	if err != nil {
		t.Fatalf("create other bot: %v", err)
	}
	started := mustUser(t, s, "+15551248991")
	neverStarted := mustUser(t, s, "+15551248992")
	const unknownID int64 = 1 << 62

	for _, targetID := range []int64{unknownID, neverStarted.ID, otherBot.ID} {
		if _, _, _, err := s.SendBotMessage(ctx, bot.ID, targetID, "must not land", 88001+targetID%97); !errors.Is(err, store.ErrBotRecipientForbidden) {
			t.Errorf("send to target %d error = %v, want ErrBotRecipientForbidden", targetID, err)
		}
	}
	botState, err := s.State(ctx, bot.ID)
	if err != nil {
		t.Fatalf("read bot state after refused sends: %v", err)
	}
	if botState.Pts != 0 {
		t.Fatalf("bot pts after refused sends = %d, want 0", botState.Pts)
	}
	for _, targetID := range []int64{unknownID, neverStarted.ID, otherBot.ID} {
		history, err := s.History(ctx, bot.ID, store.PeerTypeUser, targetID, 0, 10)
		if err != nil {
			t.Fatalf("read bot history with target %d: %v", targetID, err)
		}
		if len(history) != 0 {
			t.Errorf("bot has %d messages for refused target %d", len(history), targetID)
		}
	}

	if _, _, _, _, err := s.SendMessage(ctx, started.ID, bot.ID, "/start", 88010, 0, 0); err != nil {
		t.Fatalf("send /start: %v", err)
	}
	sender, senderPts, recipientPts, err := s.SendBotMessage(ctx, bot.ID, started.ID, "hi", 88011)
	if err != nil {
		t.Fatalf("send bot reply: %v", err)
	}
	if !sender.Out || sender.OwnerID != bot.ID || sender.PeerID != started.ID || sender.FromID != bot.ID || sender.Text != "hi" || sender.PeerLocalID <= 0 {
		t.Fatalf("bot sender row = %+v, want outgoing bot message with recipient copy", sender)
	}
	if senderPts != 2 || recipientPts != 2 {
		t.Fatalf("sender/recipient pts = %d/%d, want 2/2", senderPts, recipientPts)
	}
	received := msgAt(t, s, started.ID, sender.PeerLocalID)
	if received.Out || received.OwnerID != started.ID || received.PeerID != bot.ID || received.FromID != bot.ID || received.Text != "hi" {
		t.Fatalf("human recipient row = %+v, want incoming message from bot", received)
	}
	events, err := s.EventsSince(ctx, started.ID, 1)
	if err != nil {
		t.Fatalf("read human events: %v", err)
	}
	if len(events) != 1 || events[0].Type != store.EventNewMessage || events[0].LocalID != received.LocalID {
		t.Fatalf("human events after /start = %+v, want one reply event", events)
	}
}

func TestSendBotMessageRefusesAUserWhoBlockedTheBotWithoutWriting(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	bot, _, err := s.CreateBot(ctx, "send_blocked_test_bot", "Blocked test")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	user := mustUser(t, s, "+15551248993")
	if _, _, _, _, err := s.SendMessage(ctx, user.ID, bot.ID, "/start", 88100, 0, 0); err != nil {
		t.Fatalf("send /start: %v", err)
	}
	if _, err := s.BlockUser(ctx, user.ID, bot.ID); err != nil {
		t.Fatalf("block bot: %v", err)
	}
	beforeBot, err := s.State(ctx, bot.ID)
	if err != nil {
		t.Fatalf("read bot state before refused send: %v", err)
	}
	beforeUser, err := s.State(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user state before refused send: %v", err)
	}
	message, senderPts, recipientPts, err := s.SendBotMessage(ctx, bot.ID, user.ID, "blocked reply", 88101)
	if !errors.Is(err, store.ErrBotBlockedByUser) {
		t.Fatalf("send to blocking user error = %v, want ErrBotBlockedByUser", err)
	}
	if message.LocalID != 0 || senderPts != 0 || recipientPts != 0 {
		t.Fatalf("blocked send result = %+v with pts %d/%d, want no writes", message, senderPts, recipientPts)
	}
	afterBot, err := s.State(ctx, bot.ID)
	if err != nil {
		t.Fatalf("read bot state after refused send: %v", err)
	}
	afterUser, err := s.State(ctx, user.ID)
	if err != nil {
		t.Fatalf("read user state after refused send: %v", err)
	}
	if afterBot.Pts != beforeBot.Pts || afterUser.Pts != beforeUser.Pts {
		t.Fatalf("blocked send changed pts: bot %d/%d user %d/%d", beforeBot.Pts, afterBot.Pts, beforeUser.Pts, afterUser.Pts)
	}
	history, err := s.History(ctx, user.ID, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read user's dialog after refused send: %v", err)
	}
	if len(history) != 1 || history[0].Text != "/start" {
		t.Fatalf("user history after blocked send = %+v, want only /start", history)
	}
}
