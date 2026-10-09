package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

const ownerWireMax int64 = 1<<31 - 1

type ownerAllocatorState struct {
	pts         int64
	nextLocalID int64
}

func setOwnerAllocatorState(t *testing.T, s *store.Store, ownerID int64, state ownerAllocatorState) {
	t.Helper()
	if err := store.SetOwnerAllocatorStateForTest(context.Background(), s, ownerID, state.pts, state.nextLocalID); err != nil {
		t.Fatalf("set owner %d allocator state: %v", ownerID, err)
	}
}

func getOwnerAllocatorState(t *testing.T, s *store.Store, ownerID int64) ownerAllocatorState {
	t.Helper()
	pts, nextLocalID, err := store.OwnerAllocatorStateForTest(context.Background(), s, ownerID)
	if err != nil {
		t.Fatalf("read owner %d allocator state: %v", ownerID, err)
	}
	return ownerAllocatorState{pts: pts, nextLocalID: nextLocalID}
}

func TestOwnerWidthRefusalTwoOwnerSendRollsBackLateExhaustedCopy(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551490001")
	b := mustUser(t, s, "+15551490002")
	setOwnerAllocatorState(t, s, a.ID, ownerAllocatorState{pts: 9, nextLocalID: 41})
	setOwnerAllocatorState(t, s, b.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: 81})
	before := map[int64]ownerAllocatorState{
		a.ID: getOwnerAllocatorState(t, s, a.ID),
		b.ID: getOwnerAllocatorState(t, s, b.ID),
	}

	message, senderPts, recipientPts, dup, err := s.SendMessage(ctx, a.ID, b.ID, "must not persist", 1496001, 0, 0)
	if err == nil {
		t.Fatal("send succeeded when the recipient pts would exceed int32")
	}
	if message.LocalID != 0 || senderPts != 0 || recipientPts != 0 || dup {
		t.Errorf("refused send returned message=%+v pts=%d/%d dup=%v", message, senderPts, recipientPts, dup)
	}

	for _, owner := range []store.User{a, b} {
		if got := getOwnerAllocatorState(t, s, owner.ID); got != before[owner.ID] {
			t.Errorf("owner %d allocator state = %+v, want unchanged %+v", owner.ID, got, before[owner.ID])
		}
		events, eventErr := s.EventsSince(ctx, owner.ID, 0)
		if eventErr != nil {
			t.Fatalf("owner %d events: %v", owner.ID, eventErr)
		}
		if len(events) != 0 {
			t.Errorf("owner %d events = %+v, want none", owner.ID, events)
		}
		dialogs, dialogErr := s.Dialogs(ctx, owner.ID, 0, 100)
		if dialogErr != nil {
			t.Fatalf("owner %d dialogs: %v", owner.ID, dialogErr)
		}
		if len(dialogs) != 0 {
			t.Errorf("owner %d dialogs = %+v, want none", owner.ID, dialogs)
		}
		if _, ok, messageErr := s.MessageByOwnerLocal(ctx, owner.ID, before[owner.ID].nextLocalID); messageErr != nil || ok {
			t.Errorf("owner %d exhausted send copy: ok=%v err=%v, want absent", owner.ID, ok, messageErr)
		}
	}
}

func TestOwnerWidthRefusalExhaustedLocalIDLeavesOwnerUnchanged(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551490003")
	setOwnerAllocatorState(t, s, owner.ID, ownerAllocatorState{pts: 12, nextLocalID: ownerWireMax + 1})
	before := getOwnerAllocatorState(t, s, owner.ID)

	message, senderPts, recipientPts, dup, err := s.SendMessage(ctx, owner.ID, owner.ID, "must not persist", 1496002, 0, 0)
	if err == nil {
		t.Fatal("self send succeeded after local_id exhaustion")
	}
	if message.LocalID != 0 || senderPts != 0 || recipientPts != 0 || dup {
		t.Errorf("refused self send returned message=%+v pts=%d/%d dup=%v", message, senderPts, recipientPts, dup)
	}
	if got := getOwnerAllocatorState(t, s, owner.ID); got != before {
		t.Fatalf("allocator state = %+v, want unchanged %+v", got, before)
	}
	events, err := s.EventsSince(ctx, owner.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none", events)
	}
	dialogs, err := s.Dialogs(ctx, owner.ID, 0, 100)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	if len(dialogs) != 0 {
		t.Fatalf("dialogs = %+v, want none", dialogs)
	}
}

func TestOwnerWidthRefusalPtsOnlyRollsBackLateReadCopy(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551490004")
	b := mustUser(t, s, "+15551490005")
	send(t, s, a, b, "existing", 1496003)
	setOwnerAllocatorState(t, s, a.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: 2})
	setOwnerAllocatorState(t, s, b.ID, ownerAllocatorState{pts: 9, nextLocalID: 2})
	before := map[int64]ownerAllocatorState{
		a.ID: getOwnerAllocatorState(t, s, a.ID),
		b.ID: getOwnerAllocatorState(t, s, b.ID),
	}
	eventsBefore := map[int64][]store.Event{}
	dialogsBefore := map[int64]store.Dialog{}
	for _, pair := range [][2]int64{{a.ID, b.ID}, {b.ID, a.ID}} {
		events, eventErr := s.EventsSince(ctx, pair[0], 0)
		if eventErr != nil {
			t.Fatalf("owner %d events: %v", pair[0], eventErr)
		}
		eventsBefore[pair[0]] = events
		dialogsBefore[pair[0]] = dialogWith(t, s, pair[0], pair[1])
	}

	if _, _, err := s.ReadHistory(ctx, b.ID, a.ID, 1); err == nil {
		t.Fatal("read history succeeded when the peer pts would exceed int32")
	}

	for _, pair := range [][2]int64{{a.ID, b.ID}, {b.ID, a.ID}} {
		ownerID, peerID := pair[0], pair[1]
		if got := getOwnerAllocatorState(t, s, ownerID); got != before[ownerID] {
			t.Errorf("owner %d allocator state = %+v, want unchanged %+v", ownerID, got, before[ownerID])
		}
		events, eventErr := s.EventsSince(ctx, ownerID, 0)
		if eventErr != nil {
			t.Fatalf("owner %d events after refusal: %v", ownerID, eventErr)
		}
		if !reflect.DeepEqual(events, eventsBefore[ownerID]) {
			t.Errorf("owner %d events = %+v, want unchanged %+v", ownerID, events, eventsBefore[ownerID])
		}
		if got := dialogWith(t, s, ownerID, peerID); !reflect.DeepEqual(got, dialogsBefore[ownerID]) {
			t.Errorf("owner %d dialog = %+v, want unchanged %+v", ownerID, got, dialogsBefore[ownerID])
		}
	}
}

func TestOwnerWidthRefusalAllowsLastLegalIDAndPts(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner := mustUser(t, s, "+15551490006")
	setOwnerAllocatorState(t, s, owner.ID, ownerAllocatorState{pts: 9, nextLocalID: ownerWireMax})

	message, senderPts, recipientPts, _, err := s.SendMessage(ctx, owner.ID, owner.ID, "last id", 1496004, 0, 0)
	if err != nil {
		t.Fatalf("send with last legal local_id: %v", err)
	}
	if message.LocalID != ownerWireMax || senderPts != 10 || recipientPts != 10 {
		t.Fatalf("last legal send = local_id %d, pts %d/%d; want %d, 10/10", message.LocalID, senderPts, recipientPts, ownerWireMax)
	}
	if got, want := getOwnerAllocatorState(t, s, owner.ID), (ownerAllocatorState{pts: 10, nextLocalID: ownerWireMax + 1}); got != want {
		t.Fatalf("state after last legal local_id = %+v, want %+v", got, want)
	}
	if _, _, _, _, err = s.SendMessage(ctx, owner.ID, owner.ID, "past last id", 1496005, 0, 0); err == nil {
		t.Fatal("send succeeded after next_local_id advanced past the wire maximum")
	}
	if events, eventErr := s.EventsSince(ctx, owner.ID, 0); eventErr != nil || len(events) != 1 {
		t.Fatalf("events after cursor exhaustion = %+v err=%v, want the single committed event", events, eventErr)
	}

	a := mustUser(t, s, "+15551490007")
	b := mustUser(t, s, "+15551490008")
	send(t, s, a, b, "first", 1496006)
	send(t, s, a, b, "second", 1496007)
	setOwnerAllocatorState(t, s, b.ID, ownerAllocatorState{pts: ownerWireMax - 1, nextLocalID: 3})
	lastPts, peerPts, err := s.ReadHistory(ctx, b.ID, a.ID, 1)
	if err != nil {
		t.Fatalf("read to last legal pts: %v", err)
	}
	if lastPts != int(ownerWireMax) || peerPts != 3 {
		t.Fatalf("read pts = %d/%d, want %d/3", lastPts, peerPts, ownerWireMax)
	}
	if got, want := getOwnerAllocatorState(t, s, b.ID), (ownerAllocatorState{pts: ownerWireMax, nextLocalID: 3}); got != want {
		t.Fatalf("state after last legal pts = %+v, want %+v", got, want)
	}
	if _, _, err = s.ReadHistory(ctx, b.ID, a.ID, 2); err == nil {
		t.Fatal("read history succeeded after pts reached the wire maximum")
	}
	if got, want := getOwnerAllocatorState(t, s, b.ID), (ownerAllocatorState{pts: ownerWireMax, nextLocalID: 3}); got != want {
		t.Fatalf("state after pts exhaustion = %+v, want %+v", got, want)
	}
}
