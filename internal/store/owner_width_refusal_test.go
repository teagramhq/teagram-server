package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

type ownerVisibleSnapshot struct {
	allocator ownerAllocatorState
	messages  []store.Message
	events    []store.Event
	dialogs   []store.Dialog
}

func captureOwnerVisibleSnapshot(t *testing.T, s *store.Store, ownerID int64) ownerVisibleSnapshot {
	t.Helper()
	ctx := context.Background()
	snapshot := ownerVisibleSnapshot{allocator: getOwnerAllocatorState(t, s, ownerID)}
	dialogs, err := s.Dialogs(ctx, ownerID, 0, 1000)
	if err != nil {
		t.Fatalf("owner %d dialogs before operation: %v", ownerID, err)
	}
	snapshot.dialogs = dialogs
	for _, dialog := range dialogs {
		messages, err := s.History(ctx, ownerID, dialog.PeerType, dialog.PeerID, 0, 10000)
		if err != nil {
			t.Fatalf("owner %d history for %d/%d: %v", ownerID, dialog.PeerType, dialog.PeerID, err)
		}
		snapshot.messages = append(snapshot.messages, messages...)
	}
	events, err := s.EventsSince(ctx, ownerID, 0)
	if err != nil {
		t.Fatalf("owner %d events before operation: %v", ownerID, err)
	}
	snapshot.events = events
	return snapshot
}

func captureOwnerVisibleSnapshots(t *testing.T, s *store.Store, ownerIDs ...int64) map[int64]ownerVisibleSnapshot {
	t.Helper()
	snapshots := make(map[int64]ownerVisibleSnapshot, len(ownerIDs))
	for _, ownerID := range ownerIDs {
		snapshots[ownerID] = captureOwnerVisibleSnapshot(t, s, ownerID)
	}
	return snapshots
}

func assertOwnerVisibleSnapshotsUnchanged(t *testing.T, s *store.Store, before map[int64]ownerVisibleSnapshot) {
	t.Helper()
	for ownerID, want := range before {
		if got := captureOwnerVisibleSnapshot(t, s, ownerID); !reflect.DeepEqual(got, want) {
			t.Errorf("owner %d visible state changed: before %+v after %+v", ownerID, want, got)
		}
	}
}

func listenForUpdateNotifications(t *testing.T, s *store.Store) *pgx.Conn {
	t.Helper()
	conn, err := store.ListenUpdateNotificationsForTest(context.Background(), s)
	if err != nil {
		t.Fatalf("listen for update notifications: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close update notification listener: %v", err)
		}
	})
	return conn
}

func assertNoUpdateNotification(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	notification, err := conn.WaitForNotification(ctx)
	if err == nil {
		t.Errorf("operation emitted update notification %q", notification.Payload)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait for update notification: %v", err)
	}
}

func chatMessageCopyLocalID(t *testing.T, s *store.Store, ownerID, chatID, fanoutID int64) int64 {
	t.Helper()
	messages, err := s.History(context.Background(), ownerID, store.PeerTypeChat, chatID, 0, 10000)
	if err != nil {
		t.Fatalf("owner %d chat history: %v", ownerID, err)
	}
	for _, message := range messages {
		if message.FanoutID == fanoutID {
			return message.LocalID
		}
	}
	t.Fatalf("owner %d has no copy for fanout %d", ownerID, fanoutID)
	return 0
}

func pollViewsForOwners(t *testing.T, s *store.Store, refs map[int64]store.PollMessageRef) map[int64]store.Poll {
	t.Helper()
	views := make(map[int64]store.Poll, len(refs))
	for ownerID, ref := range refs {
		poll, err := s.PollForMessage(context.Background(), ownerID, ref)
		if err != nil {
			t.Fatalf("owner %d poll view: %v", ownerID, err)
		}
		views[ownerID] = poll
	}
	return views
}

func assertPollViewsUnchanged(t *testing.T, s *store.Store, refs map[int64]store.PollMessageRef, before map[int64]store.Poll) {
	t.Helper()
	if got := pollViewsForOwners(t, s, refs); !reflect.DeepEqual(got, before) {
		t.Errorf("poll views changed: before %+v after %+v", before, got)
	}
}

func TestOwnerWidthRefusalGroupFanoutRollsBackEarlierCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551491001")
	b := mustUser(t, s, "+15551491002")
	c := mustUser(t, s, "+15551491003")
	chat := chatWith(t, s, a, b, c)
	lastState := getOwnerAllocatorState(t, s, c.ID)
	setOwnerAllocatorState(t, s, c.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: lastState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, a.ID, b.ID, c.ID)
	listener := listenForUpdateNotifications(t, s)

	sender, perOwner, duplicate, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: a.ID, Text: "refused group fanout", RandomID: 1496101,
	})
	if err == nil {
		t.Fatal("group fanout succeeded when the last member pts would exceed int32")
	}
	if sender != (store.Message{}) || perOwner != nil || duplicate {
		t.Errorf("refused group fanout returned sender=%+v pts=%v duplicate=%v", sender, perOwner, duplicate)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalServiceFanoutRollsBackTitleAndCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551492001")
	b := mustUser(t, s, "+15551492002")
	c := mustUser(t, s, "+15551492003")
	chat := chatWith(t, s, a, b, c)
	lastState := getOwnerAllocatorState(t, s, c.ID)
	setOwnerAllocatorState(t, s, c.ID, ownerAllocatorState{pts: lastState.pts, nextLocalID: ownerWireMax + 1})
	before := captureOwnerVisibleSnapshots(t, s, a.ID, b.ID, c.ID)
	beforeChat, ok, err := s.ChatByID(ctx, chat.ID)
	if err != nil || !ok {
		t.Fatalf("load chat before title change: chat=%+v ok=%v err=%v", beforeChat, ok, err)
	}
	listener := listenForUpdateNotifications(t, s)

	updated, sender, perOwner, err := s.SetChatTitle(ctx, chat.ID, a.ID, "refused title")
	if err == nil {
		t.Fatal("service fanout succeeded when the last member local_id was exhausted")
	}
	if !reflect.DeepEqual(updated, store.Chat{}) || sender != (store.Message{}) || perOwner != nil {
		t.Errorf("refused title change returned chat=%+v sender=%+v pts=%v", updated, sender, perOwner)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	if got, exists, chatErr := s.ChatByID(ctx, chat.ID); chatErr != nil || !exists || !reflect.DeepEqual(got, beforeChat) {
		t.Errorf("chat after refused title change = %+v exists=%v err=%v, want unchanged %+v", got, exists, chatErr, beforeChat)
	}
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalForwardRollsBackSenderCopy(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	from := mustUser(t, s, "+15551493001")
	sourcePeer := mustUser(t, s, "+15551493002")
	destination := mustUser(t, s, "+15551493003")
	source := send(t, s, from, sourcePeer, "source text", 1496301)
	destState := getOwnerAllocatorState(t, s, destination.ID)
	setOwnerAllocatorState(t, s, destination.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: destState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, from.ID, sourcePeer.ID, destination.ID)
	listener := listenForUpdateNotifications(t, s)

	perOwner, forwarded, err := s.ForwardMessages(ctx, from.ID, store.PeerTypeUser, destination.ID, []store.ForwardSource{{
		FromID: source.FromID, Date: source.Date, Text: source.Text,
	}}, []int64{1496302})
	if err == nil {
		t.Fatal("forward succeeded when the destination pts would exceed int32")
	}
	if perOwner != nil || forwarded != nil {
		t.Errorf("refused forward returned pts=%v messages=%+v", perOwner, forwarded)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalChatForwardRollsBackCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	from := mustUser(t, s, "+15551493101")
	sourcePeer := mustUser(t, s, "+15551493102")
	member := mustUser(t, s, "+15551493103")
	last := mustUser(t, s, "+15551493104")
	chat := chatWith(t, s, from, member, last)
	source := send(t, s, from, sourcePeer, "source text", 1496311)
	lastState := getOwnerAllocatorState(t, s, last.ID)
	setOwnerAllocatorState(t, s, last.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: lastState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, from.ID, sourcePeer.ID, member.ID, last.ID)
	listener := listenForUpdateNotifications(t, s)

	perOwner, forwarded, err := s.ForwardMessages(ctx, from.ID, store.PeerTypeChat, chat.ID, []store.ForwardSource{{
		FromID: source.FromID, Date: source.Date, Text: source.Text,
	}}, []int64{1496312})
	if err == nil {
		t.Fatal("chat forward succeeded when an entitled owner's pts would exceed int32")
	}
	if perOwner != nil || forwarded != nil {
		t.Errorf("refused chat forward returned pts=%v messages=%+v", perOwner, forwarded)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalChatEditRollsBackEarlierCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551494001")
	b := mustUser(t, s, "+15551494002")
	c := mustUser(t, s, "+15551494003")
	chat := chatWith(t, s, a, b, c)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "original", RandomID: 1496401})
	lastState := getOwnerAllocatorState(t, s, c.ID)
	setOwnerAllocatorState(t, s, c.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: lastState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, a.ID, b.ID, c.ID)
	listener := listenForUpdateNotifications(t, s)

	peerID, pts, err := s.EditMessage(ctx, a.ID, message.LocalID, "edited")
	if err == nil {
		t.Fatal("chat edit succeeded when the last copy pts would exceed int32")
	}
	if peerID != 0 || pts != 0 {
		t.Errorf("refused chat edit returned peer=%d pts=%d", peerID, pts)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalChatDeleteRollsBackEarlierCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551495001")
	b := mustUser(t, s, "+15551495002")
	c := mustUser(t, s, "+15551495003")
	chat := chatWith(t, s, a, b, c)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "delete me", RandomID: 1496501})
	lastState := getOwnerAllocatorState(t, s, c.ID)
	setOwnerAllocatorState(t, s, c.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: lastState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, a.ID, b.ID, c.ID)
	listener := listenForUpdateNotifications(t, s)

	perOwner, err := s.DeleteMessages(ctx, a.ID, []int64{message.LocalID}, true)
	if err == nil {
		t.Fatal("chat delete succeeded when the last copy pts would exceed int32")
	}
	if perOwner != nil {
		t.Errorf("refused chat delete returned pts=%v", perOwner)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalChatReadReceiptRollsBackEarlierOwners(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551496001")
	b := mustUser(t, s, "+15551496002")
	reader := mustUser(t, s, "+15551496003")
	chat := chatWith(t, s, a, b, reader)
	sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "from first sender", RandomID: 1496601})
	sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: b.ID, Text: "from last sender", RandomID: 1496602})
	readerHistory, err := s.History(ctx, reader.ID, store.PeerTypeChat, chat.ID, 0, 100)
	if err != nil || len(readerHistory) == 0 {
		t.Fatalf("reader history = %+v err=%v", readerHistory, err)
	}
	lastState := getOwnerAllocatorState(t, s, b.ID)
	setOwnerAllocatorState(t, s, b.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: lastState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, a.ID, b.ID, reader.ID)
	listener := listenForUpdateNotifications(t, s)

	result, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, readerHistory[0].LocalID)
	if err == nil {
		t.Fatal("chat read receipt succeeded when a later sender pts would exceed int32")
	}
	if !reflect.DeepEqual(result, store.ChatReadHistoryResult{}) {
		t.Errorf("refused chat read returned %+v", result)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalPollVoteRollsBackSharedVote(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15551497001")
	voter := mustUser(t, s, "+15551497002")
	last := mustUser(t, s, "+15551497003")
	chat := chatWith(t, s, creator, voter, last)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: creator.ID, Text: "poll", RandomID: 1496701})
	creatorRef := store.PollMessageRef{PeerType: store.PeerTypeChat, PeerID: chat.ID, LocalID: message.LocalID}
	if _, duplicate, err := s.CreatePoll(ctx, creator.ID, creatorRef, ordinaryPollDraft()); err != nil || duplicate {
		t.Fatalf("create poll = duplicate %v err %v", duplicate, err)
	}
	refs := map[int64]store.PollMessageRef{}
	for _, owner := range []store.User{creator, voter, last} {
		refs[owner.ID] = store.PollMessageRef{
			PeerType: store.PeerTypeChat, PeerID: chat.ID,
			LocalID: chatMessageCopyLocalID(t, s, owner.ID, chat.ID, message.FanoutID),
		}
	}
	lastState := getOwnerAllocatorState(t, s, last.ID)
	setOwnerAllocatorState(t, s, last.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: lastState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, creator.ID, voter.ID, last.ID)
	pollsBefore := pollViewsForOwners(t, s, refs)
	listener := listenForUpdateNotifications(t, s)

	_, ownerPts, changed, err := s.CastPollVoteWithUpdates(ctx, voter.ID, refs[voter.ID], [][]byte{[]byte("a")})
	if err == nil {
		t.Fatal("poll vote succeeded when the last entitled owner pts would exceed int32")
	}
	if ownerPts != nil || changed {
		t.Errorf("refused poll vote returned pts=%v changed=%v", ownerPts, changed)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertPollViewsUnchanged(t, s, refs, pollsBefore)
	assertNoUpdateNotification(t, listener)
}

func TestOwnerWidthRefusalPollCloseRollsBackClosedStateAndCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15551498001")
	member := mustUser(t, s, "+15551498002")
	last := mustUser(t, s, "+15551498003")
	chat := chatWith(t, s, creator, member, last)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: creator.ID, Text: "poll", RandomID: 1496801})
	creatorRef := store.PollMessageRef{PeerType: store.PeerTypeChat, PeerID: chat.ID, LocalID: message.LocalID}
	if _, duplicate, err := s.CreatePoll(ctx, creator.ID, creatorRef, ordinaryPollDraft()); err != nil || duplicate {
		t.Fatalf("create poll = duplicate %v err %v", duplicate, err)
	}
	refs := map[int64]store.PollMessageRef{}
	for _, owner := range []store.User{creator, member, last} {
		refs[owner.ID] = store.PollMessageRef{
			PeerType: store.PeerTypeChat, PeerID: chat.ID,
			LocalID: chatMessageCopyLocalID(t, s, owner.ID, chat.ID, message.FanoutID),
		}
	}
	lastState := getOwnerAllocatorState(t, s, last.ID)
	setOwnerAllocatorState(t, s, last.ID, ownerAllocatorState{pts: ownerWireMax, nextLocalID: lastState.nextLocalID})
	before := captureOwnerVisibleSnapshots(t, s, creator.ID, member.ID, last.ID)
	pollsBefore := pollViewsForOwners(t, s, refs)
	listener := listenForUpdateNotifications(t, s)

	changed, ownerPts, err := s.ClosePollWithUpdates(ctx, creator.ID, refs[creator.ID])
	if err == nil {
		t.Fatal("poll close succeeded when the last entitled owner pts would exceed int32")
	}
	if changed || ownerPts != nil {
		t.Errorf("refused poll close returned changed=%v pts=%v", changed, ownerPts)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	assertPollViewsUnchanged(t, s, refs, pollsBefore)
	assertNoUpdateNotification(t, listener)
}
