package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

// TestSetChatPinnedMessageRecordsMemberEvents proves a chat pin and unpin each
// spend one pts per member and record an event naming that member's own copy,
// and that a request matching the current state records nothing.
func TestSetChatPinnedMessageRecordsMemberEvents(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551870001")
	b := mustUser(t, s, "+15551870002")
	ch, err := s.CreateChat(ctx, a.ID, "pins", []int64{b.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	sender, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: ch.ID, FromID: a.ID, Text: "pin me", RandomID: 1})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	bCopy := lastNewMessage(t, s, b.ID)

	before := map[int64]int{a.ID: statePts(t, s, a.ID), b.ID: statePts(t, s, b.ID)}
	pinnedID := int32(sender.LocalID) //nolint:gosec // local_id fits int32
	_, change, err := s.SetChatPinnedMessage(ctx, ch.ID, a.ID, &pinnedID)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if !change.Changed {
		t.Fatal("pin reported no change")
	}
	wantLocal := map[int64]int64{a.ID: sender.LocalID, b.ID: bCopy}
	for owner, local := range wantLocal {
		if got := change.OwnerPts[owner]; got != before[owner]+1 {
			t.Fatalf("owner %d pin pts = %d, want %d", owner, got, before[owner]+1)
		}
		assertOnlyEvent(t, s, owner, before[owner], store.Event{Pts: before[owner] + 1, Type: store.EventPin, LocalID: local})
	}

	if _, change, err = s.SetChatPinnedMessage(ctx, ch.ID, a.ID, &pinnedID); err != nil {
		t.Fatalf("repeat pin: %v", err)
	}
	if change.Changed || len(change.OwnerPts) != 0 {
		t.Fatalf("repeat pin change = %+v, want none", change)
	}

	if _, change, err = s.SetChatPinnedMessage(ctx, ch.ID, a.ID, nil); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	for owner, local := range wantLocal {
		if got := change.OwnerPts[owner]; got != before[owner]+2 {
			t.Fatalf("owner %d unpin pts = %d, want %d", owner, got, before[owner]+2)
		}
		assertOnlyEvent(t, s, owner, before[owner]+1, store.Event{Pts: before[owner] + 2, Type: store.EventUnpin, LocalID: local})
	}

	if _, change, err = s.SetChatPinnedMessage(ctx, ch.ID, a.ID, nil); err != nil {
		t.Fatalf("repeat unpin: %v", err)
	}
	if change.Changed {
		t.Fatalf("repeat unpin change = %+v, want none", change)
	}
	for owner := range wantLocal {
		if got := statePts(t, s, owner); got != before[owner]+2 {
			t.Fatalf("owner %d pts after no-ops = %d, want %d", owner, got, before[owner]+2)
		}
	}
}

// TestSetChannelPinnedMessageRecordsChannelEvents proves a channel pin and
// unpin each spend one channel pts and record an event naming the post, and
// that a request matching the current state records nothing.
func TestSetChannelPinnedMessageRecordsChannelEvents(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	author := mustUser(t, s, "+15551870003")
	ch := mustChannel(t, s, author.ID, "pins").ID
	post, _, _, err := s.PostChannelMessage(ctx, ch, author.ID, "pin me", 1, nil, 0)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	before, err := s.ChannelState(ctx, ch)
	if err != nil {
		t.Fatalf("channel state: %v", err)
	}

	pinnedID := int32(post.LocalID) //nolint:gosec // local_id fits int32
	_, change, err := s.SetChannelPinnedMessage(ctx, ch, author.ID, &pinnedID)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if change != (store.ChannelPinChange{Changed: true, MessageID: pinnedID, Pts: before + 1}) {
		t.Fatalf("pin change = %+v, want pts %d", change, before+1)
	}
	if _, change, err = s.SetChannelPinnedMessage(ctx, ch, author.ID, &pinnedID); err != nil || change.Changed {
		t.Fatalf("repeat pin change = %+v err = %v, want none", change, err)
	}
	if _, change, err = s.SetChannelPinnedMessage(ctx, ch, author.ID, nil); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if change != (store.ChannelPinChange{Changed: true, MessageID: pinnedID, Pts: before + 2}) {
		t.Fatalf("unpin change = %+v, want pts %d", change, before+2)
	}
	if _, change, err = s.SetChannelPinnedMessage(ctx, ch, author.ID, nil); err != nil || change.Changed {
		t.Fatalf("repeat unpin change = %+v err = %v, want none", change, err)
	}

	events, err := s.ChannelEventsWindow(ctx, ch, before, before+10, 10)
	if err != nil {
		t.Fatalf("channel events: %v", err)
	}
	want := []store.ChannelEvent{
		{Pts: before + 1, Type: store.EventPin, LocalID: post.LocalID},
		{Pts: before + 2, Type: store.EventUnpin, LocalID: post.LocalID},
	}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Fatalf("channel events = %+v, want %+v", events, want)
	}
}

func statePts(t *testing.T, s *store.Store, userID int64) int {
	t.Helper()
	st, err := s.StateWithoutUnread(context.Background(), userID)
	if err != nil {
		t.Fatalf("state %d: %v", userID, err)
	}
	return st.Pts
}

func lastNewMessage(t *testing.T, s *store.Store, userID int64) int64 {
	t.Helper()
	events, err := s.EventsSince(context.Background(), userID, 0)
	if err != nil {
		t.Fatalf("events %d: %v", userID, err)
	}
	for _, ev := range slices.Backward(events) {
		if ev.Type == store.EventNewMessage {
			return ev.LocalID
		}
	}
	t.Fatalf("owner %d has no new-message event", userID)
	return 0
}

func assertOnlyEvent(t *testing.T, s *store.Store, owner int64, fromPts int, want store.Event) {
	t.Helper()
	events, err := s.EventsSince(context.Background(), owner, fromPts)
	if err != nil {
		t.Fatalf("events %d: %v", owner, err)
	}
	if len(events) != 1 || events[0] != want {
		t.Fatalf("owner %d events after %d = %+v, want [%+v]", owner, fromPts, events, want)
	}
}
