package api_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPeerUserIDValidatesAccessHash(t *testing.T) {
	t.Parallel()

	// Derived hash for (viewer=5, peer=5) is the valid hash.
	peer := api.InputPeerUser(5, 5)
	id, err := api.PeerUserID(peer, 5)
	if err != nil || id != 5 {
		t.Fatalf("valid peer: id=%d err=%v", id, err)
	}

	// access_hash == user_id (M1 placeholder) must be rejected.
	if _, err := api.PeerUserID(&tg.InputPeerUser{UserID: 5, AccessHash: 5}, 5); err == nil {
		t.Error("M1 placeholder: expected PEER_ID_INVALID, got nil")
	}
	if id, err := api.PeerUserID(&tg.InputPeerSelf{}, 5); err != nil || id != 5 {
		t.Fatalf("authenticated self peer: id=%d err=%v, want id 5", id, err)
	}
	if _, err := api.PeerUserID(&tg.InputPeerSelf{}, 0); err == nil {
		t.Error("unauthenticated self peer: expected PEER_ID_INVALID, got nil")
	}

	for name, peer := range map[string]tg.InputPeerClass{
		"wrong hash":           &tg.InputPeerUser{UserID: 5, AccessHash: 6},
		"cross-account replay": &tg.InputPeerUser{UserID: 5, AccessHash: api.DeriveUserHash(999, 5)},
		"zero id":              &tg.InputPeerUser{UserID: 0, AccessHash: 0},
		"chat":                 &tg.InputPeerChat{ChatID: 1},
	} {
		if _, err := api.PeerUserID(peer, 5); err == nil {
			t.Errorf("%s: expected PEER_ID_INVALID, got nil", name)
		}
	}
}

// testBlobs opens a blob store rooted in the test's own temporary directory.
func testBlobs(t *testing.T) blob.Store {
	t.Helper()
	b, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return b
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, _ := openStoreDSN(t)
	return s
}

// openStoreDSN opens a store and hands back the DSN it runs on, for a test that
// also needs a raw connection to the same database. Each pgtest.DSN call clones
// a fresh database, so the DSN has to come from the same call as the store.
func openStoreDSN(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := pgtest.DSN(t)
	s, err := store.Open(context.Background(), dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s, dsn
}

func TestHandleSendMessagePersistsAndReturnsUpdates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551291001")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551291002")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}

	enc, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Message:  "hello",
		RandomID: 4242,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	ups, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("result type = %T, want *tg.Updates", enc)
	}
	var sawMsgID, sawNewMsg bool
	for _, u := range ups.Updates {
		switch up := u.(type) {
		case *tg.UpdateMessageID:
			if up.RandomID == 4242 {
				sawMsgID = true
			}
		case *tg.UpdateNewMessage:
			if m, ok := up.Message.(*tg.Message); ok && m.Message == "hello" && m.Out {
				sawNewMsg = true
			}
		}
	}
	if !sawMsgID || !sawNewMsg {
		t.Fatalf("updates missing pieces: msgID=%v newMsg=%v", sawMsgID, sawNewMsg)
	}

	// Recipient got the inbox copy.
	recv, ok, err := s.MessageByOwnerLocal(ctx, b.ID, 1)
	if err != nil || !ok {
		t.Fatalf("recipient copy: ok=%v err=%v", ok, err)
	}
	if recv.Text != "hello" || recv.Out {
		t.Fatalf("recipient copy wrong: %+v", recv)
	}
}

func TestGetMessagesResolvesOwnerLocalReplyReferencesInRequestOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551291301")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551291302")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	peer := api.InputPeerUser(a.ID, b.ID)

	targetResult, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "quoted target", RandomID: 91301,
	})
	if err != nil {
		t.Fatalf("send target: %v", err)
	}
	target := messageOf(t, targetResult)

	replyRequest := &tg.MessagesSendMessageRequest{Peer: peer, Message: "reply source", RandomID: 91302}
	replyRequest.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: target.ID})
	replyResult, err := api.SendMessageForTest(s, a.ID, replyRequest)
	if err != nil {
		t.Fatalf("send reply source: %v", err)
	}
	replySource := messageOf(t, replyResult)
	stateABefore, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("read caller state before getMessages: %v", err)
	}
	stateBBefore, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("read peer state before getMessages: %v", err)
	}

	result, err := api.GetMessagesForTest(s, a.ID, &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{
		&tg.InputMessageReplyTo{ID: replySource.ID},
		&tg.InputMessageID{ID: replySource.ID},
		&tg.InputMessageID{ID: target.ID},
		&tg.InputMessageReplyTo{ID: replySource.ID},
		&tg.InputMessageID{ID: 999999},
		&tg.InputMessageID{ID: -7},
	}})
	if err != nil {
		t.Fatalf("getMessages: %v", err)
	}
	response, ok := result.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("getMessages result = %T, want *tg.MessagesMessages", result)
	}
	if len(response.Messages) != 5 {
		t.Fatalf("getMessages count = %d, want 5", len(response.Messages))
	}
	resolvedReply, ok := response.Messages[0].(*tg.Message)
	if !ok || resolvedReply.ID != target.ID || resolvedReply.Message != "quoted target" {
		t.Errorf("reply reference = %#v, want target (%d, %q)", response.Messages[0], target.ID, "quoted target")
	}
	directSource, ok := response.Messages[1].(*tg.Message)
	if !ok || directSource.ID != replySource.ID || directSource.Message != "reply source" {
		t.Errorf("direct reference = %#v, want source (%d, %q)", response.Messages[1], replySource.ID, "reply source")
	}
	directTarget, ok := response.Messages[2].(*tg.Message)
	if !ok || directTarget.ID != target.ID || directTarget.Message != "quoted target" {
		t.Errorf("direct target = %#v, want target (%d, %q)", response.Messages[2], target.ID, "quoted target")
	}
	for index, id := range []int{999999, -7} {
		empty, ok := response.Messages[index+3].(*tg.MessageEmpty)
		if !ok || empty.ID != id {
			t.Errorf("message %d = %#v, want messageEmpty{%d}", index+3, response.Messages[index+3], id)
		}
	}
	stateAAfter, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("read caller state after getMessages: %v", err)
	}
	stateBAfter, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("read peer state after getMessages: %v", err)
	}
	if stateABefore != stateAAfter || stateBBefore != stateBAfter {
		t.Errorf("getMessages changed update state: caller %v -> %v, peer %v -> %v", stateABefore, stateAAfter, stateBBefore, stateBAfter)
	}
}

func TestHandleSendMessageUnauthorized(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	_, err := api.SendMessageForTest(s, 0, &tg.MessagesSendMessageRequest{
		Peer:    &tg.InputPeerUser{UserID: 1, AccessHash: 1},
		Message: "x",
	})
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unbound send: got %v, want AUTH_KEY_UNREGISTERED", err)
	}
}

// rpcError asserts err is an RPC error carrying msg.
func rpcError(t *testing.T, err error, msg string) {
	t.Helper()
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != msg {
		t.Fatalf("got %v, want %s", err, msg)
	}
}

// chatWith creates one user per phone and a chat owned by the first of them,
// with every other user as a member.
func chatWith(t *testing.T, s *store.Store, phones ...string) ([]store.User, store.Chat) {
	t.Helper()
	ctx := context.Background()
	users := make([]store.User, len(phones))
	for i, p := range phones {
		u, err := s.CreateUser(ctx, p)
		if err != nil {
			t.Fatalf("user %s: %v", p, err)
		}
		users[i] = u
	}
	members := make([]int64, 0, len(users)-1)
	for _, u := range users[1:] {
		members = append(members, u.ID)
	}
	chat, err := s.CreateChat(ctx, users[0].ID, "Crew", members)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return users, chat
}

func TestHandleSendMessageToChatFansOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292001", "+15551292002", "+15551292003")

	// Pull one member's pts off the sender's before the fan-out, so an envelope
	// that echoed another owner's entry from perOwner cannot coincide with the
	// sender's own.
	if _, err := api.SendMessageForTest(s, users[1].ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(users[1].ID, users[2].ID), Message: "dm", RandomID: 1,
	}); err != nil {
		t.Fatalf("pre-advance: %v", err)
	}

	enc, err := api.SendMessageForTest(s, users[0].ID, &tg.MessagesSendMessageRequest{
		Peer:     &tg.InputPeerChat{ChatID: chat.ID},
		Message:  "hi",
		RandomID: 42,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	ups, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("result type = %T, want *tg.Updates", enc)
	}
	// The envelope must name the sender's own copy and the sender's own new pts.
	sent, herr := s.History(ctx, users[0].ID, store.PeerTypeChat, chat.ID, 0, 1)
	if herr != nil || len(sent) != 1 {
		t.Fatalf("sender copy: %+v err=%v", sent, herr)
	}
	senderState, serr := s.State(ctx, users[0].ID)
	if serr != nil {
		t.Fatalf("sender state: %v", serr)
	}

	var sawMsgID, sawNewMsg bool
	for _, u := range ups.Updates {
		switch up := u.(type) {
		case *tg.UpdateMessageID:
			if up.RandomID != 42 {
				t.Errorf("updateMessageID random_id = %d, want 42", up.RandomID)
			}
			if int64(up.ID) != sent[0].LocalID {
				t.Errorf("updateMessageID id = %d, want %d", up.ID, sent[0].LocalID)
			}
			sawMsgID = true
		case *tg.UpdateNewMessage:
			m, isMsg := up.Message.(*tg.Message)
			if !isMsg {
				continue
			}
			peer, isChat := m.PeerID.(*tg.PeerChat)
			from, isUser := m.FromID.(*tg.PeerUser)
			sawNewMsg = isChat && peer.ChatID == chat.ID &&
				isUser && from.UserID == users[0].ID && m.Out && m.Message == "hi"
			if int64(m.ID) != sent[0].LocalID {
				t.Errorf("updateNewMessage id = %d, want %d", m.ID, sent[0].LocalID)
			}
			if up.Pts != senderState.Pts {
				t.Errorf("updateNewMessage pts = %d, want the sender's %d", up.Pts, senderState.Pts)
			}
			if up.PtsCount != 1 {
				t.Errorf("updateNewMessage pts_count = %d, want 1", up.PtsCount)
			}
		}
	}
	if !sawMsgID || !sawNewMsg {
		t.Fatalf("updates missing pieces: msgID=%v newMsg=%v", sawMsgID, sawNewMsg)
	}
	if len(ups.Chats) != 1 {
		t.Fatalf("chats = %d, want 1", len(ups.Chats))
	}
	if c, isChat := ups.Chats[0].(*tg.Chat); !isChat || c.ID != chat.ID {
		t.Fatalf("chat entry = %#v, want live chat %d", ups.Chats[0], chat.ID)
	}

	// One row per member, and every member's pts advanced by the send.
	for _, u := range users {
		msgs, herr := s.History(ctx, u.ID, store.PeerTypeChat, chat.ID, 0, 100)
		if herr != nil {
			t.Fatalf("history %d: %v", u.ID, herr)
		}
		if len(msgs) != 1 || msgs[0].Text != "hi" || msgs[0].FromID != users[0].ID {
			t.Fatalf("user %d copies = %+v", u.ID, msgs)
		}
		if msgs[0].Out != (u.ID == users[0].ID) {
			t.Errorf("user %d out = %v", u.ID, msgs[0].Out)
		}
	}
}

func TestHandleSendMessageToChatIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	users, chat := chatWith(t, s, "+15551292011", "+15551292012", "+15551292013")

	// A resend must cost nothing a client can observe, and a lost !dup guard shows
	// up as a second round of update nudges long before it shows up in the rows.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err = conn.Exec(ctx, "LISTEN "+store.ChannelUpdates); err != nil {
		t.Fatalf("listen: %v", err)
	}

	peer := &tg.InputPeerChat{ChatID: chat.ID}
	first, err := api.SendMessageForTest(s, users[0].ID, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "hi", RandomID: 42,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// One nudge per member for the send that did write.
	notified := make(map[string]bool, len(users))
	for range users {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		n, werr := conn.WaitForNotification(waitCtx)
		cancel()
		if werr != nil {
			t.Fatalf("wait notification: %v", werr)
		}
		notified[n.Payload] = true
	}
	for _, u := range users {
		if !notified[strconv.FormatInt(u.ID, 10)] {
			t.Errorf("no update nudge for member %d", u.ID)
		}
	}
	before := make(map[int64]int, len(users))
	for _, u := range users {
		st, serr := s.State(ctx, u.ID)
		if serr != nil {
			t.Fatalf("state %d: %v", u.ID, serr)
		}
		before[u.ID] = st.Pts
	}

	second, err := api.SendMessageForTest(s, users[0].ID, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "hi", RandomID: 42,
	})
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if msgID(t, first) != msgID(t, second) {
		t.Errorf("resend id = %d, want %d", msgID(t, second), msgID(t, first))
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if n, werr := conn.WaitForNotification(waitCtx); werr == nil {
		t.Errorf("resend emitted a nudge for %q", n.Payload)
	} else if !errors.Is(werr, context.DeadlineExceeded) {
		t.Fatalf("wait after resend: %v", werr)
	}
	for _, u := range users {
		st, serr := s.State(ctx, u.ID)
		if serr != nil {
			t.Fatalf("state %d: %v", u.ID, serr)
		}
		if st.Pts != before[u.ID] {
			t.Errorf("user %d pts = %d, want %d", u.ID, st.Pts, before[u.ID])
		}
		msgs, herr := s.History(ctx, u.ID, store.PeerTypeChat, chat.ID, 0, 100)
		if herr != nil {
			t.Fatalf("history %d: %v", u.ID, herr)
		}
		if len(msgs) != 1 {
			t.Errorf("user %d rows = %d, want 1", u.ID, len(msgs))
		}
	}
}

// msgID pulls the sender's local id out of an updateMessageID envelope.
func msgID(t *testing.T, enc bin.Encoder) int {
	t.Helper()
	ups, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("result type = %T, want *tg.Updates", enc)
	}
	for _, u := range ups.Updates {
		if m, isID := u.(*tg.UpdateMessageID); isID {
			return m.ID
		}
	}
	t.Fatal("no updateMessageID")
	return 0
}

func TestHandleChatRPCsRejectNonMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292021", "+15551292022")
	outsider, err := s.CreateUser(ctx, "+15551292023")
	if err != nil {
		t.Fatalf("outsider: %v", err)
	}

	// A chat the caller is not in and a chat that does not exist are the same error.
	for _, chatID := range []int64{chat.ID, chat.ID + 10_000, -1} {
		_, serr := api.SendMessageForTest(s, outsider.ID, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: "probe", RandomID: 7,
		})
		rpcError(t, serr, "PEER_ID_INVALID")
		_, herr := api.GetHistoryForTest(s, outsider.ID, &tg.MessagesGetHistoryRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID},
		})
		rpcError(t, herr, "PEER_ID_INVALID")
	}

	// Nothing was written for anyone.
	for _, u := range append(users, outsider) {
		msgs, herr := s.History(ctx, u.ID, store.PeerTypeChat, chat.ID, 0, 100)
		if herr != nil {
			t.Fatalf("history %d: %v", u.ID, herr)
		}
		if len(msgs) != 0 {
			t.Errorf("user %d rows = %d, want 0", u.ID, len(msgs))
		}
	}
}

func TestHandleGetHistoryOnChatListsEveryAuthor(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292031", "+15551292032", "+15551292033")
	peer := &tg.InputPeerChat{ChatID: chat.ID}

	for i, u := range users {
		if _, err := api.SendMessageForTest(s, u.ID, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: "m", RandomID: int64(100 + i),
		}); err != nil {
			t.Fatalf("send %d: %v", u.ID, err)
		}
	}

	enc, err := api.GetHistoryForTest(s, users[0].ID, &tg.MessagesGetHistoryRequest{Peer: peer})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	res, ok := enc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesMessages", enc)
	}
	if len(res.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(res.Messages))
	}
	// Newest-first.
	prev := 1 << 30
	for _, m := range res.Messages {
		msg, isMsg := m.(*tg.Message)
		if !isMsg {
			t.Fatalf("message type = %T", m)
		}
		if msg.ID >= prev {
			t.Fatalf("ids not descending: %d after %d", msg.ID, prev)
		}
		prev = msg.ID
	}
	got := make(map[int64]bool, len(res.Users))
	for _, u := range res.Users {
		got[u.GetID()] = true
	}
	for _, u := range users {
		if !got[u.ID] {
			t.Errorf("user list missing author %d", u.ID)
		}
	}
	if len(res.Chats) != 1 {
		t.Errorf("chats = %d, want 1", len(res.Chats))
	}
}

func TestHandleGetHistoryCapsOrdinalPageAtServerMaximum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	caller, err := s.CreateUser(ctx, "+15551292061")
	if err != nil {
		t.Fatalf("caller: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15551292062")
	if err != nil {
		t.Fatalf("peer: %v", err)
	}
	for i := range 105 {
		if _, _, _, _, err := s.SendMessage(ctx, caller.ID, peer.ID, fmt.Sprintf("message-%03d", i), int64(1061+i), 0, 0); err != nil {
			t.Fatalf("send message %d: %v", i, err)
		}
	}

	enc, err := api.GetHistoryForTest(s, caller.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(caller.ID, peer.ID), OffsetID: 0, AddOffset: 1, Limit: 1000,
	})
	if err != nil {
		t.Fatalf("getHistory: %v", err)
	}
	history, ok := enc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("getHistory result = %T, want *tg.MessagesMessages", enc)
	}
	if len(history.Messages) != 100 {
		t.Fatalf("history length = %d, want server maximum 100", len(history.Messages))
	}
	newest, ok := history.Messages[0].(*tg.Message)
	if !ok || newest.Message != "message-103" {
		t.Fatalf("first history message = %T/%v, want message-103 after positive add_offset", history.Messages[0], history.Messages[0])
	}
}

func TestHandleGetHistoryKeepsMembershipAndHydrationInOneSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users := make([]store.User, 3)
	for i, phone := range []string{"+15551292051", "+15551292052", "+15551292053"} {
		user, err := s.CreateUser(ctx, phone)
		if err != nil {
			t.Fatalf("user %s: %v", phone, err)
		}
		users[i] = user
	}
	created, err := api.CreateChatForTest(s, users[0].ID, &tg.MessagesCreateChatRequest{
		Title: "Crew", Users: inputUsers(users[0].ID, users[1].ID, users[2].ID),
	})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	invited, ok := created.(*tg.MessagesInvitedUsers)
	if !ok {
		t.Fatalf("create chat result = %T, want *tg.MessagesInvitedUsers", created)
	}
	updates, ok := invited.Updates.(*tg.Updates)
	if !ok {
		t.Fatalf("create chat updates = %T, want *tg.Updates", invited.Updates)
	}
	if len(updates.Chats) != 1 {
		t.Fatalf("create chat updates have %d chats, want one", len(updates.Chats))
	}
	group, ok := updates.Chats[0].(*tg.Chat)
	if !ok {
		t.Fatalf("created chat = %T, want *tg.Chat", updates.Chats[0])
	}
	chatID := group.ID
	newMember, err := s.CreateUser(ctx, "+15551292054")
	if err != nil {
		t.Fatalf("new member: %v", err)
	}
	if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chatID, FromID: users[1].ID, Text: "history during snapshot", RandomID: 4052,
	}); err != nil {
		t.Fatalf("send history message: %v", err)
	}

	var mutationErr error
	var mutationRan bool
	store.SetChatHistorySnapshotHook(s, func() {
		store.SetChatHistorySnapshotHook(s, nil)
		removed, _, _, err := s.RemoveChatUser(ctx, chatID, users[1].ID, users[0].ID)
		if err != nil {
			mutationErr = err
			return
		}
		if !removed {
			mutationErr = errors.New("snapshot hook did not remove the existing member")
			return
		}
		removed, _, _, err = s.RemoveChatUser(ctx, chatID, users[2].ID, users[0].ID)
		if err != nil {
			mutationErr = err
			return
		}
		if !removed {
			mutationErr = errors.New("snapshot hook did not remove the second existing member")
			return
		}
		added, _, _, err := s.AddChatUser(ctx, chatID, newMember.ID, users[0].ID)
		if err != nil {
			mutationErr = err
			return
		}
		if !added {
			mutationErr = errors.New("snapshot hook did not add the new member")
			return
		}
		mutationRan = true
	})
	t.Cleanup(func() { store.SetChatHistorySnapshotHook(s, nil) })

	enc, err := api.GetHistoryForTest(s, users[1].ID, &tg.MessagesGetHistoryRequest{
		Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("getHistory: %v", err)
	}
	if mutationErr != nil {
		t.Fatalf("concurrent membership mutation: %v", mutationErr)
	}
	if !mutationRan {
		t.Fatal("getHistory did not run the membership snapshot hook")
	}
	history, ok := enc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("getHistory result = %T, want *tg.MessagesMessages", enc)
	}

	var sawHistoryMessage, sawCreate bool
	for _, message := range history.Messages {
		if msg, ok := message.(*tg.Message); ok && msg.Message == "history during snapshot" {
			sawHistoryMessage = true
		}
		service, ok := message.(*tg.MessageService)
		if !ok {
			continue
		}
		create, ok := service.Action.(*tg.MessageActionChatCreate)
		if !ok {
			continue
		}
		sawCreate = true
		participantIDs := make(map[int64]bool, len(create.Users))
		for _, id := range create.Users {
			participantIDs[id] = true
		}
		if !participantIDs[users[1].ID] || participantIDs[newMember.ID] {
			t.Errorf("create action users = %v, want removed member %d and no new member %d", create.Users, users[1].ID, newMember.ID)
		}
	}
	if !sawHistoryMessage || !sawCreate {
		t.Fatalf("history has message=%t create=%t, want both", sawHistoryMessage, sawCreate)
	}

	var sawRemovedMemberProfile, sawNewMember bool
	for _, user := range history.Users {
		switch u := user.(type) {
		case *tg.User:
			if u.ID == users[2].ID {
				sawRemovedMemberProfile = true
			}
			if u.ID == newMember.ID {
				sawNewMember = true
			}
		case *tg.UserEmpty:
			if u.ID == newMember.ID {
				sawNewMember = true
			}
		}
	}
	if !sawRemovedMemberProfile {
		t.Error("removed member profile is missing from the request's original entitlement snapshot")
	}
	if sawNewMember {
		t.Error("new member appears in the request's original user snapshot")
	}
}

func TestHandleGetDialogsMixesUsersAndChats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292041", "+15551292042")
	other, err := s.CreateUser(ctx, "+15551292043")
	if err != nil {
		t.Fatalf("other: %v", err)
	}

	if _, err = api.SendMessageForTest(s, users[1].ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(users[1].ID, other.ID), Message: "1:1", RandomID: 1,
	}); err != nil {
		t.Fatalf("dm: %v", err)
	}
	// The group's last message is from the other member, so the caller can only
	// render it if Users carries the top message's author.
	if _, err = api.SendMessageForTest(s, users[0].ID, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "group", RandomID: 2,
	}); err != nil {
		t.Fatalf("chat send: %v", err)
	}

	enc, err := api.GetDialogsForTest(s, users[1].ID)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	if len(res.Dialogs) != 2 {
		t.Fatalf("dialogs = %d, want 2", len(res.Dialogs))
	}
	var sawUser, sawChat bool
	for _, d := range res.Dialogs {
		switch p := d.(*tg.Dialog).Peer.(type) {
		case *tg.PeerUser:
			sawUser = p.UserID == other.ID
		case *tg.PeerChat:
			sawChat = p.ChatID == chat.ID
		}
	}
	if !sawUser || !sawChat {
		t.Fatalf("peers: user=%v chat=%v", sawUser, sawChat)
	}
	if len(res.Chats) != 1 {
		t.Fatalf("chats = %d, want 1", len(res.Chats))
	}
	c, isChat := res.Chats[0].(*tg.Chat)
	if !isChat || c.Title != "Crew" {
		t.Fatalf("chat entry = %#v, want live chat titled Crew", res.Chats[0])
	}
	got := make(map[int64]bool, len(res.Users))
	for _, u := range res.Users {
		got[u.GetID()] = true
	}
	for _, id := range []int64{users[0].ID, users[1].ID, other.ID} {
		if !got[id] {
			t.Errorf("user list missing %d", id)
		}
	}
}

// A service row names user ids in its action, and a client renders them as
// unknown users unless the enclosing Users covers them.
func TestHandleGetHistoryOnChatListsServiceActionUsers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292071", "+15551292072")
	joiner, err := s.CreateUser(ctx, "+15551292073")
	if err != nil {
		t.Fatalf("joiner: %v", err)
	}
	if _, _, _, err = s.AddChatUser(ctx, chat.ID, joiner.ID, users[0].ID); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, _, _, err = s.RemoveChatUser(ctx, chat.ID, joiner.ID, users[0].ID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	enc, err := api.GetHistoryForTest(s, users[1].ID, &tg.MessagesGetHistoryRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID},
	})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	res, ok := enc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesMessages", enc)
	}
	var sawAdd, sawDelete bool
	for _, m := range res.Messages {
		svc, isSvc := m.(*tg.MessageService)
		if !isSvc {
			continue
		}
		switch a := svc.Action.(type) {
		case *tg.MessageActionChatAddUser:
			sawAdd = len(a.Users) == 1 && a.Users[0] == joiner.ID
		case *tg.MessageActionChatDeleteUser:
			sawDelete = a.UserID == joiner.ID
		}
	}
	if !sawAdd || !sawDelete {
		t.Fatalf("service rows: add=%v delete=%v", sawAdd, sawDelete)
	}
	got := make(map[int64]bool, len(res.Users))
	for _, u := range res.Users {
		got[u.GetID()] = true
	}
	if !got[joiner.ID] {
		t.Errorf("user list missing action subject %d", joiner.ID)
	}
}

func TestHandleGetDialogsHidesChatAfterRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292051", "+15551292052")

	if _, err := api.SendMessageForTest(s, users[0].ID, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "before", RandomID: 5,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, _, _, err := s.RemoveChatUser(ctx, chat.ID, users[1].ID, users[0].ID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	enc, err := api.GetDialogsForTest(s, users[1].ID)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	if len(res.Chats) != 1 {
		t.Fatalf("chats = %d, want 1", len(res.Chats))
	}
	forbidden, ok := res.Chats[0].(*tg.ChatForbidden)
	if !ok {
		t.Fatalf("chat entry = %#v, want *tg.ChatForbidden", res.Chats[0])
	}
	if forbidden.ID != chat.ID {
		t.Errorf("forbidden id = %d, want %d", forbidden.ID, chat.ID)
	}
	// The dialog row survives removal by design; only the metadata is withheld.
	if len(res.Dialogs) != 1 {
		t.Errorf("dialogs = %d, want 1", len(res.Dialogs))
	}
}

// When the top message of a chat dialog is an add-user service row, the user
// named by action_user_id must appear in the Users list so the client can
// render the participant instead of an unknown placeholder. Without the fix,
// Users only contains the author and the viewer.
func TestHandleGetDialogsListsAddUserActionSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292081", "+15551292082")
	joiner, err := s.CreateUser(ctx, "+15551292083")
	if err != nil {
		t.Fatalf("joiner: %v", err)
	}
	if _, _, _, err = s.AddChatUser(ctx, chat.ID, joiner.ID, users[0].ID); err != nil {
		t.Fatalf("add: %v", err)
	}

	enc, err := api.GetDialogsForTest(s, users[1].ID)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	got := make(map[int64]bool, len(res.Users))
	for _, u := range res.Users {
		got[u.GetID()] = true
	}
	for _, id := range []int64{users[0].ID, users[1].ID, joiner.ID} {
		if !got[id] {
			t.Errorf("user list missing %d", id)
		}
	}
}

// When the top message of a chat dialog is a delete-user service row, the
// removed user must appear in the Users list. Without the fix, the removed
// user is absent and the client renders an unknown placeholder.
func TestHandleGetDialogsListsDeleteUserActionSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292091", "+15551292092", "+15551292093")
	if _, _, _, err := s.RemoveChatUser(ctx, chat.ID, users[2].ID, users[0].ID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	enc, err := api.GetDialogsForTest(s, users[1].ID)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	got := make(map[int64]bool, len(res.Users))
	for _, u := range res.Users {
		got[u.GetID()] = true
	}
	for _, id := range []int64{users[0].ID, users[1].ID, users[2].ID} {
		if !got[id] {
			t.Errorf("user list missing %d", id)
		}
	}
}

// When the only message in a chat is the create row, the top message on the
// dialog must carry the participant ids, and every participant must appear in
// Users. Without the fix, the action renders with an empty user list.
func TestHandleGetDialogsOnCreateRowListsParticipants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292101", "+15551292102", "+15551292103")
	// CreateChat writes no message; handleCreateChat announces the create row,
	// and that fan-out is what gives each member a dialog.
	if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: users[0].ID, Text: "Crew", Action: store.ChatActionCreate,
	}); err != nil {
		t.Fatalf("create row: %v", err)
	}

	enc, err := api.GetDialogsForTest(s, users[1].ID)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	got := make(map[int64]bool, len(res.Users))
	for _, u := range res.Users {
		got[u.GetID()] = true
	}
	for _, u := range users {
		if !got[u.ID] {
			t.Errorf("user list missing participant %d", u.ID)
		}
	}
	// Top message should be the create action with users populated.
	if len(res.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(res.Messages))
	}
	svc, ok := res.Messages[0].(*tg.MessageService)
	if !ok {
		t.Fatalf("top message type = %T, want *tg.MessageService", res.Messages[0])
	}
	createAction, ok := svc.Action.(*tg.MessageActionChatCreate)
	if !ok {
		t.Fatalf("action type = %T, want *tg.MessageActionChatCreate", svc.Action)
	}
	if len(createAction.Users) != 3 {
		t.Errorf("create action users = %d, want 3", len(createAction.Users))
	}
}

// A viewer removed from a chat still gets their dialog row, and RemoveChatUser
// fans the announcement to the removed user, so their top message is the
// delete-user row naming them. Without the fix ActionUserID never enters the
// user list. The chat itself stays degraded to tg.ChatForbidden.
func TestHandleGetDialogsRemovedViewerSeesDeleteRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292111", "+15551292112")
	if _, _, _, err := s.RemoveChatUser(ctx, chat.ID, users[1].ID, users[0].ID); err != nil {
		t.Fatalf("remove: %v", err)
	}

	enc, err := api.GetDialogsForTest(s, users[1].ID)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	// Chat should be forbidden.
	if len(res.Chats) != 1 {
		t.Fatalf("chats = %d, want 1", len(res.Chats))
	}
	forbidden, ok := res.Chats[0].(*tg.ChatForbidden)
	if !ok {
		t.Fatalf("chat entry = %#v, want *tg.ChatForbidden", res.Chats[0])
	}
	if forbidden.ID != chat.ID {
		t.Errorf("forbidden id = %d, want %d", forbidden.ID, chat.ID)
	}
	// The dialog row survives removal.
	if len(res.Dialogs) != 1 {
		t.Errorf("dialogs = %d, want 1", len(res.Dialogs))
	}
	// Top message is the delete-user announcement naming the removed viewer.
	if len(res.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(res.Messages))
	}
	svc, ok := res.Messages[0].(*tg.MessageService)
	if !ok {
		t.Fatalf("top message type = %T, want *tg.MessageService", res.Messages[0])
	}
	del, ok := svc.Action.(*tg.MessageActionChatDeleteUser)
	if !ok {
		t.Fatalf("action type = %T, want *tg.MessageActionChatDeleteUser", svc.Action)
	}
	if del.UserID != users[1].ID {
		t.Errorf("action user = %d, want %d", del.UserID, users[1].ID)
	}
	got := make(map[int64]bool, len(res.Users))
	for _, u := range res.Users {
		got[u.GetID()] = true
	}
	if !got[users[1].ID] {
		t.Errorf("user list missing removed viewer %d", users[1].ID)
	}
}

// Criterion 4, the authorization gate itself: a non-member whose dialog top
// message is still the create row must not get the chat's live member list.
// No RPC produces that state — RemoveChatUser fans a delete row that becomes
// the removed account's top message — so the membership row is dropped
// directly, leaving the retained dialog pointing at the create row.
func TestHandleGetDialogsNonMemberOnCreateRowGetsNoParticipants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	users, chat := chatWith(t, s, "+15551292121", "+15551292122", "+15551292123")
	// Fan the create row while the viewer is still a member: SendChatMessage
	// writes dialog rows only for current members and rejects a non-member
	// sender, so this cannot move below the DELETE.
	if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: users[0].ID, Text: "Crew", Action: store.ChatActionCreate,
	}); err != nil {
		t.Fatalf("create row: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err = conn.Exec(ctx,
		"DELETE FROM chat_participants WHERE chat_id = $1 AND user_id = $2", chat.ID, users[1].ID,
	); err != nil {
		t.Fatalf("drop membership: %v", err)
	}

	enc, err := api.GetDialogsForTest(s, users[1].ID)
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	// The dialog row survives, degraded to a forbidden chat.
	if len(res.Dialogs) != 1 {
		t.Errorf("dialogs = %d, want 1", len(res.Dialogs))
	}
	if len(res.Chats) != 1 {
		t.Fatalf("chats = %d, want 1", len(res.Chats))
	}
	forbidden, ok := res.Chats[0].(*tg.ChatForbidden)
	if !ok {
		t.Fatalf("chat entry = %#v, want *tg.ChatForbidden", res.Chats[0])
	}
	if forbidden.ID != chat.ID {
		t.Errorf("forbidden id = %d, want %d", forbidden.ID, chat.ID)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(res.Messages))
	}
	svc, ok := res.Messages[0].(*tg.MessageService)
	if !ok {
		t.Fatalf("top message type = %T, want *tg.MessageService", res.Messages[0])
	}
	createAction, ok := svc.Action.(*tg.MessageActionChatCreate)
	if !ok {
		t.Fatalf("action type = %T, want *tg.MessageActionChatCreate", svc.Action)
	}
	// The gate: no Participants call, so no member ids reach a non-member.
	if len(createAction.Users) != 0 {
		t.Errorf("create action users = %d, want 0 for a non-member", len(createAction.Users))
	}
	for _, u := range res.Users {
		if u.GetID() == users[2].ID {
			t.Errorf("user list leaks member %d to a non-member", users[2].ID)
		}
	}
}

func TestSetTypingAcceptsChatPeers(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292061", "+15551292062")

	result, err := api.SetTypingForTest(s, users[0].ID, &tg.MessagesSetTypingRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Action: &tg.SendMessageTypingAction{},
	})
	if err != nil {
		t.Fatalf("setTyping as chat member: %v", err)
	}
	if _, ok := result.(*tg.BoolTrue); !ok {
		t.Fatalf("setTyping result = %T, want *tg.BoolTrue", result)
	}
}

func TestSetTypingRejectsChatNonMembers(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292063", "+15551292064")
	outsider, err := s.CreateUser(context.Background(), "+15551292065")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}

	_, err = api.SetTypingForTest(s, outsider.ID, &tg.MessagesSetTypingRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Action: &tg.SendMessageTypingAction{},
	})
	rpcError(t, err, "PEER_ID_INVALID")

	_, err = api.SetTypingForTest(s, users[0].ID, &tg.MessagesSetTypingRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID + 1000}, Action: &tg.SendMessageTypingAction{},
	})
	rpcError(t, err, "PEER_ID_INVALID")
}

func TestSetTypingIsRateLimitedPerAccount(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551292066", "+15551292067")
	request := &tg.MessagesSetTypingRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Action: &tg.SendMessageTypingAction{},
	}
	limit := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	if _, err := api.SetTypingForTestWithRateLimit(s, users[0].ID, request, limit); err != nil {
		t.Fatalf("first setTyping: %v", err)
	}
	_, err := api.SetTypingForTestWithRateLimit(s, users[0].ID, request, limit)
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 420 || !strings.HasPrefix(rpc.Message, "FLOOD_WAIT_") {
		t.Fatalf("second setTyping error = %v, want FLOOD_WAIT", err)
	}
}

// TestSendAndEditMessageRejectUnstorableText pins the API boundary against text
// Postgres cannot store. A NUL byte or an invalid UTF-8 sequence reaches the
// driver intact and fails the INSERT, so without this guard a client bug is a
// 500 and an error log line an unprivileged caller can repeat at will.
func TestSendAndEditMessageRejectUnstorableText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551293001")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551293002")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	peer := api.InputPeerUser(a.ID, b.ID)

	for name, text := range map[string]string{
		"nul byte":     "a\x00b",
		"invalid utf8": "\xff",
	} {
		_, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: text, RandomID: 1,
		})
		rpcError(t, err, "MESSAGE_EMPTY")
		msgs, err := s.History(ctx, a.ID, store.PeerTypeUser, b.ID, 0, 100)
		if err != nil {
			t.Fatalf("%s: history: %v", name, err)
		}
		if len(msgs) != 0 {
			t.Fatalf("%s: stored %d messages, want 0", name, len(msgs))
		}
	}

	// A stored message must survive an edit carrying the same text.
	enc, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "hello", RandomID: 2,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	assertEncodes(t, enc)
	sent, ok, err := s.MessageByOwnerLocal(ctx, a.ID, 1)
	if err != nil || !ok {
		t.Fatalf("sent message: ok=%v err=%v", ok, err)
	}

	_, err = api.EditMessageForTest(s, a.ID, &tg.MessagesEditMessageRequest{
		Peer: peer, ID: int(sent.LocalID), Message: "\xff",
	})
	rpcError(t, err, "MESSAGE_EMPTY")
	after, ok, err := s.MessageByOwnerLocal(ctx, a.ID, sent.LocalID)
	if err != nil || !ok {
		t.Fatalf("message after edit: ok=%v err=%v", ok, err)
	}
	if after.Text != "hello" {
		t.Fatalf("text = %q, want hello", after.Text)
	}
}

// TestSendAndEditMessageKeepMultiByteText pins that the guard rejects only what
// the database cannot store: valid multi-byte UTF-8 round-trips unchanged.
func TestSendAndEditMessageKeepMultiByteText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551293011")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551293012")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}

	peer := api.InputPeerUser(a.ID, b.ID)

	const text = "Привет 👋"
	enc, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  text,
		RandomID: 7,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	assertEncodes(t, enc)
	sent, ok, err := s.MessageByOwnerLocal(ctx, a.ID, 1)
	if err != nil || !ok {
		t.Fatalf("sent message: ok=%v err=%v", ok, err)
	}
	if sent.Text != text {
		t.Fatalf("stored text = %q, want %q", sent.Text, text)
	}

	const edited = "Пока 👋"
	if _, err := api.EditMessageForTest(s, a.ID, &tg.MessagesEditMessageRequest{
		Peer: peer, ID: int(sent.LocalID), Message: edited,
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	after, ok, err := s.MessageByOwnerLocal(ctx, a.ID, sent.LocalID)
	if err != nil || !ok {
		t.Fatalf("message after edit: ok=%v err=%v", ok, err)
	}
	if after.Text != edited {
		t.Fatalf("edited text = %q, want %q", after.Text, edited)
	}
}

// dialogsFor gives owner n DM dialogs, top messages 1..n, newest last.
func dialogsFor(t *testing.T, s *store.Store, phonePrefix string, n int) store.User {
	t.Helper()
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, phonePrefix+"000")
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	for i := range n {
		peer, perr := s.CreateUser(ctx, phonePrefix+strconv.Itoa(101+i))
		if perr != nil {
			t.Fatalf("peer %d: %v", i, perr)
		}
		// The peer sends, so the owner gets an inbox copy and a dialog.
		if _, perr = api.SendMessageForTest(s, peer.ID, &tg.MessagesSendMessageRequest{
			Peer: api.InputPeerUser(peer.ID, owner.ID), Message: "hi", RandomID: int64(i + 1),
		}); perr != nil {
			t.Fatalf("dm %d: %v", i, perr)
		}
	}
	return owner
}

// topMessage reads a reply dialog's top message.
func topMessage(t *testing.T, d tg.DialogClass) int {
	t.Helper()
	dlg, ok := d.(*tg.Dialog)
	if !ok {
		t.Fatalf("dialog type = %T, want *tg.Dialog", d)
	}
	return dlg.TopMessage
}

// A list longer than the page must come back as a slice carrying the whole-list
// total, so the client knows there is more behind the page it got.
func TestHandleGetDialogsTruncatesToSlice(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	const total = 21 // one over the default limit
	owner := dialogsFor(t, s, "+1555129321", total)

	// Limit 0 clamps to defaultDialogsLimit.
	enc, err := api.GetDialogsPageForTest(s, owner.ID, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}})
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	res, ok := enc.(*tg.MessagesDialogsSlice)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogsSlice", enc)
	}
	if len(res.Dialogs) != 20 {
		t.Fatalf("dialogs = %d, want 20", len(res.Dialogs))
	}
	if res.Count != total {
		t.Fatalf("count = %d, want %d", res.Count, total)
	}
	// Newest first, so the page starts at the last dialog written.
	if top := topMessage(t, res.Dialogs[0]); top != total {
		t.Fatalf("first dialog top_message = %d, want %d", top, total)
	}

	// Paging past the first page reaches the end and drops back to the plain reply.
	last := topMessage(t, res.Dialogs[len(res.Dialogs)-1])
	enc, err = api.GetDialogsPageForTest(s, owner.ID, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{}, OffsetID: last, Limit: 20,
	})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	rest, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("second page type = %T, want *tg.MessagesDialogs", enc)
	}
	if len(rest.Dialogs) != total-20 {
		t.Fatalf("second page = %d dialogs, want %d", len(rest.Dialogs), total-20)
	}
	if got := topMessage(t, rest.Dialogs[0]); got != last-1 {
		t.Fatalf("second page starts at %d, want %d — offset is not strictly older", got, last-1)
	}
}

// An over-large limit clamps rather than serving the whole list unbounded.
func TestHandleGetDialogsClampsLimit(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	owner := dialogsFor(t, s, "+1555129331", 3)

	enc, err := api.GetDialogsPageForTest(s, owner.ID, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{}, Limit: 500,
	})
	if err != nil {
		t.Fatalf("dialogs: %v", err)
	}
	// Three dialogs is short of the clamped 100, so the reply stays plain.
	res, ok := enc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesDialogs", enc)
	}
	if len(res.Dialogs) != 3 {
		t.Fatalf("dialogs = %d, want 3", len(res.Dialogs))
	}

	// A limit the list exactly fills is a full page, so it must advertise the total.
	enc, err = api.GetDialogsPageForTest(s, owner.ID, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{}, Limit: 3,
	})
	if err != nil {
		t.Fatalf("exact page: %v", err)
	}
	slice, ok := enc.(*tg.MessagesDialogsSlice)
	if !ok {
		t.Fatalf("exact page type = %T, want *tg.MessagesDialogsSlice", enc)
	}
	if slice.Count != 3 || len(slice.Dialogs) != 3 {
		t.Fatalf("exact page = %d dialogs, count %d, want 3 and 3", len(slice.Dialogs), slice.Count)
	}
}

// mediaMessage sends a message from one user to another carrying a stored file,
// and returns the sender's row and the file it attached.
func mediaMessage(t *testing.T, s *store.Store, from, to store.User, text string, stored bool) (store.Message, store.File) {
	t.Helper()
	ctx := context.Background()
	f, err := s.AllocateFile(ctx, from.ID, 11, "text/plain", "hello.txt", 1<<31)
	if err != nil {
		t.Fatalf("allocate file: %v", err)
	}
	if stored {
		if err := s.MarkFileStored(ctx, f.ID); err != nil {
			t.Fatalf("mark stored: %v", err)
		}
	}
	sender, _, _, _, err := s.SendMessage(ctx, from.ID, to.ID, text, 909, f.ID, 0) //nolint:dogsled // only the sender row is needed here
	if err != nil {
		t.Fatalf("send media message: %v", err)
	}
	return sender, f
}

// mediaUsers creates a pair of accounts for a media test.
func mediaUsers(t *testing.T, s *store.Store, a, b string) (store.User, store.User) {
	t.Helper()
	ctx := context.Background()
	u1, err := s.CreateUser(ctx, a)
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	u2, err := s.CreateUser(ctx, b)
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	return u1, u2
}

// historyMessages runs getHistory for caller against peer and returns the page.
func historyMessages(t *testing.T, s *store.Store, caller, peer store.User) []tg.MessageClass {
	t.Helper()
	enc, err := api.GetHistoryForTest(s, caller.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(caller.ID, peer.ID),
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	res, ok := enc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("history type = %T, want *tg.MessagesMessages", enc)
	}
	return res.Messages
}

func TestHandleGetHistoryRendersDocumentMedia(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	u1, u2 := mediaUsers(t, s, "+15551294001", "+15551294002")
	_, f := mediaMessage(t, s, u1, u2, "here", true)

	msgs := historyMessages(t, s, u2, u1)
	if len(msgs) != 1 {
		t.Fatalf("history = %d messages, want 1", len(msgs))
	}
	m, ok := msgs[0].(*tg.Message)
	if !ok {
		t.Fatalf("message type = %T, want *tg.Message", msgs[0])
	}
	if m.Message != "here" {
		t.Fatalf("text = %q, want %q", m.Message, "here")
	}
	doc := mediaDocument(t, m)
	if doc.ID != f.ID || doc.AccessHash != f.AccessHash {
		t.Fatalf("document id/hash = %d/%d, want %d/%d", doc.ID, doc.AccessHash, f.ID, f.AccessHash)
	}
	if doc.MimeType != "text/plain" || doc.Size != 11 {
		t.Fatalf("document mime/size = %q/%d, want text/plain/11", doc.MimeType, doc.Size)
	}
	if len(doc.Attributes) != 1 {
		t.Fatalf("attributes = %d, want 1", len(doc.Attributes))
	}
	name, ok := doc.Attributes[0].(*tg.DocumentAttributeFilename)
	if !ok || name.FileName != "hello.txt" {
		t.Fatalf("attribute = %+v, want DocumentAttributeFilename hello.txt", doc.Attributes[0])
	}
}

func TestHandleGetHistoryUnstoredFileRendersPlainMessage(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	u1, u2 := mediaUsers(t, s, "+15551294003", "+15551294004")
	mediaMessage(t, s, u1, u2, "here", false)

	msgs := historyMessages(t, s, u2, u1)
	if len(msgs) != 1 {
		t.Fatalf("history = %d messages, want 1", len(msgs))
	}
	m, ok := msgs[0].(*tg.Message)
	if !ok {
		t.Fatalf("message type = %T, want *tg.Message", msgs[0])
	}
	if m.Media != nil {
		t.Fatalf("media = %+v, want none for an unstored file", m.Media)
	}
	if m.Message != "here" {
		t.Fatalf("text = %q, want %q", m.Message, "here")
	}
}

// mediaDocument asserts a wire message carries document media and returns it.
func mediaDocument(t *testing.T, m *tg.Message) *tg.Document {
	t.Helper()
	media, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("media type = %T, want *tg.MessageMediaDocument", m.Media)
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		t.Fatalf("document type = %T, want *tg.Document", media.Document)
	}
	return doc
}

func TestSearchMatchesInboundMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551295001")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551295002")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}

	// A sends "hello world" to B.
	if _, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Message:  "hello world",
		RandomID: 1,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// B searches for "hello" — should return the inbound message from A.
	enc, err := api.SearchForTest(s, b.ID, &tg.MessagesSearchRequest{
		Peer:   api.InputPeerUser(b.ID, a.ID),
		Q:      "hello",
		Filter: &tg.InputMessagesFilterEmpty{},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	res, ok := enc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesMessages", enc)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("B search hello: got %d messages, want 1", len(res.Messages))
	}
	m, ok := res.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("message type = %T, want *tg.Message", res.Messages[0])
	}
	if m.Message != "hello world" {
		t.Fatalf("message text = %q, want %q", m.Message, "hello world")
	}
	if m.Out {
		t.Error("inbound message should have out=false")
	}
}

func TestSearchChatPeerReturnsBothDirections(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551295011", "+15551295012")

	// A sends "quarterly report" to the chat.
	if _, err := api.SendMessageForTest(s, users[0].ID, &tg.MessagesSendMessageRequest{
		Peer:     &tg.InputPeerChat{ChatID: chat.ID},
		Message:  "quarterly report",
		RandomID: 1,
	}); err != nil {
		t.Fatalf("send A: %v", err)
	}
	// B sends "review quarterly" to the chat.
	if _, err := api.SendMessageForTest(s, users[1].ID, &tg.MessagesSendMessageRequest{
		Peer:     &tg.InputPeerChat{ChatID: chat.ID},
		Message:  "review quarterly",
		RandomID: 2,
	}); err != nil {
		t.Fatalf("send B: %v", err)
	}

	// B searches chat for "quarterly" — should return both messages.
	enc, err := api.SearchForTest(s, users[1].ID, &tg.MessagesSearchRequest{
		Peer:   &tg.InputPeerChat{ChatID: chat.ID},
		Q:      "quarterly",
		Filter: &tg.InputMessagesFilterEmpty{},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	res, ok := enc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("result type = %T, want *tg.MessagesMessages", enc)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("B search chat quarterly: got %d messages, want 2", len(res.Messages))
	}
	// Newest-first: B's own "review quarterly" then A's "quarterly report".
	got := make([]string, len(res.Messages))
	for i, m := range res.Messages {
		if msg, ok := m.(*tg.Message); ok {
			got[i] = msg.Message
		} else {
			t.Fatalf("message %d type = %T", i, m)
		}
	}
	if got[0] != "review quarterly" {
		t.Errorf("search[0] = %q, want %q", got[0], "review quarterly")
	}
	if got[1] != "quarterly report" {
		t.Errorf("search[1] = %q, want %q", got[1], "quarterly report")
	}
	// Chats should be populated for chat peers.
	if len(res.Chats) != 1 {
		t.Fatalf("chats = %d, want 1", len(res.Chats))
	}
	c, ok := res.Chats[0].(*tg.Chat)
	if !ok {
		t.Fatalf("chat type = %T, want *tg.Chat", res.Chats[0])
	}
	if c.ID != chat.ID {
		t.Errorf("chat id = %d, want %d", c.ID, chat.ID)
	}
}

func TestSearchChatPeerRejectsNonMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	_, chat := chatWith(t, s, "+15551295021", "+15551295022")
	outsider, err := s.CreateUser(ctx, "+15551295023")
	if err != nil {
		t.Fatalf("outsider: %v", err)
	}

	_, err = api.SearchForTest(s, outsider.ID, &tg.MessagesSearchRequest{
		Peer:   &tg.InputPeerChat{ChatID: chat.ID},
		Q:      "hello",
		Filter: &tg.InputMessagesFilterEmpty{},
	})
	rpcError(t, err, "PEER_ID_INVALID")
}

// When a member searches a chat by a word from its title, the create service
// row returned by search must carry the same participant list that getHistory
// returns for that same row. Without the createUsers lookup in chatSearch, the
// create action renders with an empty user list.
func TestSearchChatByTitleListsParticipants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551295041", "+15551295042", "+15551295043")
	// CreateChat writes no message; fan the create row so each member has it.
	if _, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: users[0].ID, Text: "Crew", Action: store.ChatActionCreate,
	}); err != nil {
		t.Fatalf("create row: %v", err)
	}

	// getHistory for a member must return the create row with participants.
	histEnc, err := api.GetHistoryForTest(s, users[1].ID, &tg.MessagesGetHistoryRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID},
	})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	hist, ok := histEnc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("history type = %T, want *tg.MessagesMessages", histEnc)
	}
	if len(hist.Messages) != 1 {
		t.Fatalf("history messages = %d, want 1", len(hist.Messages))
	}
	histSvc, ok := hist.Messages[0].(*tg.MessageService)
	if !ok {
		t.Fatalf("history message type = %T, want *tg.MessageService", hist.Messages[0])
	}
	histCreate, ok := histSvc.Action.(*tg.MessageActionChatCreate)
	if !ok {
		t.Fatalf("history action type = %T, want *tg.MessageActionChatCreate", histSvc.Action)
	}

	// Search by a word from the title — the create row's text is "Crew".
	searchEnc, err := api.SearchForTest(s, users[1].ID, &tg.MessagesSearchRequest{
		Peer:   &tg.InputPeerChat{ChatID: chat.ID},
		Q:      "Crew",
		Filter: &tg.InputMessagesFilterEmpty{},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	search, ok := searchEnc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("search type = %T, want *tg.MessagesMessages", searchEnc)
	}
	if len(search.Messages) != 1 {
		t.Fatalf("search messages = %d, want 1", len(search.Messages))
	}
	searchSvc, ok := search.Messages[0].(*tg.MessageService)
	if !ok {
		t.Fatalf("search message type = %T, want *tg.MessageService", search.Messages[0])
	}
	searchCreate, ok := searchSvc.Action.(*tg.MessageActionChatCreate)
	if !ok {
		t.Fatalf("search action type = %T, want *tg.MessageActionChatCreate", searchSvc.Action)
	}

	// Both paths must return the same participant list.
	if len(searchCreate.Users) != len(histCreate.Users) {
		t.Fatalf("search create users = %d, history = %d", len(searchCreate.Users), len(histCreate.Users))
	}
	histUsers := make(map[int64]bool)
	for _, id := range histCreate.Users {
		histUsers[id] = true
	}
	searchUsers := make(map[int64]bool)
	for _, id := range searchCreate.Users {
		searchUsers[id] = true
	}
	for _, u := range users {
		if !histUsers[u.ID] {
			t.Errorf("history user list missing participant %d", u.ID)
		}
		if !searchUsers[u.ID] {
			t.Errorf("search user list missing participant %d", u.ID)
		}
	}
	// The action's user ids must also appear in the Users list so clients can
	// resolve them to tg.User objects. Deleting the createUsers fan-out into
	// authors keeps the action assertion green but leaves users unresolvable.
	searchUserList := make(map[int64]bool, len(search.Users))
	for _, u := range search.Users {
		searchUserList[u.GetID()] = true
	}
	for _, u := range users {
		if !searchUserList[u.ID] {
			t.Errorf("search.Users missing participant %d", u.ID)
		}
	}
}

// channelIDFromCreate extracts the channel id from the *tg.Updates returned by
// CreateChannelForTest. Fatally fails t if the response is not parseable.
func channelIDFromCreate(t *testing.T, enc bin.Encoder) int64 {
	t.Helper()
	ups, ok := enc.(*tg.Updates)
	if !ok || len(ups.Chats) == 0 {
		t.Fatalf("create channel: unexpected response %T", enc)
	}
	ch, ok := ups.Chats[0].(*tg.Channel)
	if !ok {
		t.Fatalf("create channel: chats[0] is %T, want *tg.Channel", ups.Chats[0])
	}
	return ch.ID
}

// channelPostIDFromSend extracts the new channel message id from the *tg.Updates
// returned by SendMessageForTest. Fatally fails t if the response is not parseable.
func channelPostIDFromSend(t *testing.T, enc bin.Encoder) int {
	t.Helper()
	ups, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("send channel message: unexpected response %T", enc)
	}
	for _, u := range ups.Updates {
		if nm, ok := u.(*tg.UpdateNewChannelMessage); ok {
			if m, ok := nm.Message.(*tg.Message); ok {
				return m.ID
			}
		}
	}
	t.Fatal("send channel message: no UpdateNewChannelMessage in updates")
	return 0
}

func TestSendMessageCrossPeerReplyRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	a, err := s.CreateUser(ctx, "+15553530001")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15553530002")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	chat, err := s.CreateChat(ctx, a.ID, "Crew", []int64{b.ID})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	chRes, err := api.CreateChannelForTest(s, a.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "News"})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	chID := channelIDFromCreate(t, chRes)

	// Post a real message to the channel so the store's own reply-existence check
	// would accept it. Only the cross-peer guard can then produce MESSAGE_ID_INVALID.
	postEnc, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChannel(a.ID, chID), Message: "root", RandomID: 4,
	})
	if err != nil {
		t.Fatalf("channel root post: %v", err)
	}
	chPostID := channelPostIDFromSend(t, postEnc)

	t.Run("user", func(t *testing.T) {
		// ReplyToPeerID names a chat, not the user destination.
		req := &tg.MessagesSendMessageRequest{
			Peer: api.InputPeerUser(a.ID, b.ID), Message: "hi", RandomID: 1,
		}
		replyTo := &tg.InputReplyToMessage{ReplyToMsgID: 1}
		replyTo.SetReplyToPeerID(&tg.InputPeerChat{ChatID: chat.ID})
		req.SetReplyTo(replyTo)
		_, err := api.SendMessageForTest(s, a.ID, req)
		rpcError(t, err, "MESSAGE_ID_INVALID")
	})

	t.Run("chat", func(t *testing.T) {
		// ReplyToPeerID names a user, not the chat destination.
		req := &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "hi", RandomID: 2,
		}
		replyTo := &tg.InputReplyToMessage{ReplyToMsgID: 1}
		replyTo.SetReplyToPeerID(api.InputPeerUser(a.ID, b.ID))
		req.SetReplyTo(replyTo)
		_, err := api.SendMessageForTest(s, a.ID, req)
		rpcError(t, err, "MESSAGE_ID_INVALID")
	})

	t.Run("channel", func(t *testing.T) {
		// ReplyToPeerID names a user, not the channel destination.
		// chPostID is a real post, so only the guard produces MESSAGE_ID_INVALID.
		req := &tg.MessagesSendMessageRequest{
			Peer: api.InputPeerChannel(a.ID, chID), Message: "hi", RandomID: 3,
		}
		replyTo := &tg.InputReplyToMessage{ReplyToMsgID: chPostID}
		replyTo.SetReplyToPeerID(api.InputPeerUser(a.ID, b.ID))
		req.SetReplyTo(replyTo)
		_, err := api.SendMessageForTest(s, a.ID, req)
		rpcError(t, err, "MESSAGE_ID_INVALID")
	})
}

func TestSendMessageRejectsExplicitNonpositiveReplyWithoutWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15553530101")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15553530102")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	before, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state before: %v", err)
	}
	beforeB, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("recipient state before: %v", err)
	}

	for i, id := range []int{0, -1} {
		req := &tg.MessagesSendMessageRequest{
			Peer: api.InputPeerUser(a.ID, b.ID), Message: "invalid reply", RandomID: int64(3201 + i),
		}
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: id})
		_, err = api.SendMessageForTest(s, a.ID, req)
		rpcError(t, err, "MESSAGE_ID_INVALID")
		if _, ok, lookupErr := s.MessageByRandomID(ctx, a.ID, int64(3201+i)); lookupErr != nil || ok {
			t.Errorf("random_id %d after invalid reply: found=%v err=%v", 3201+i, ok, lookupErr)
		}
	}
	after, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state after: %v", err)
	}
	if after.Pts != before.Pts {
		t.Errorf("sender pts = %d after nonpositive replies, want unchanged %d", after.Pts, before.Pts)
	}
	afterB, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("recipient state after: %v", err)
	}
	if afterB.Pts != beforeB.Pts {
		t.Errorf("recipient pts = %d after nonpositive replies, want unchanged %d", afterB.Pts, beforeB.Pts)
	}
}

func TestSendMessageRejectsNonActiveAndCrossDialogReplyTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15553530121")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15553530122")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	c, err := s.CreateUser(ctx, "+15553530123")
	if err != nil {
		t.Fatalf("user c: %v", err)
	}
	otherDialog, _, _, _, err := s.SendMessage(ctx, a.ID, c.ID, "other dialog", 3221, 0, 0) //nolint:dogsled // only the target row and error are needed
	if err != nil {
		t.Fatalf("seed other dialog: %v", err)
	}

	for i, targetID := range []int64{999999, otherDialog.LocalID} {
		req := &tg.MessagesSendMessageRequest{
			Peer: api.InputPeerUser(a.ID, b.ID), Message: "invalid reply", RandomID: int64(3222 + i),
		}
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(targetID)})
		_, err = api.SendMessageForTest(s, a.ID, req)
		rpcError(t, err, "MESSAGE_ID_INVALID")
	}

	target, _, _, _, err := s.SendMessage(ctx, a.ID, b.ID, "target", 3224, 0, 0) //nolint:dogsled // only the target row and error are needed
	if err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if _, err := s.DeleteMessages(ctx, a.ID, []int64{target.LocalID}, false); err != nil {
		t.Fatalf("delete sender target copy: %v", err)
	}
	req := &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: "reply to deleted target", RandomID: 3225,
	}
	req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(target.LocalID)})
	_, err = api.SendMessageForTest(s, a.ID, req)
	rpcError(t, err, "MESSAGE_ID_INVALID")

	chat, err := s.CreateChat(ctx, a.ID, "Reply group", []int64{b.ID})
	if err != nil {
		t.Fatalf("create reply group: %v", err)
	}
	_, service, _, err := s.AddChatUser(ctx, chat.ID, c.ID, a.ID)
	if err != nil {
		t.Fatalf("add group member: %v", err)
	}
	groupReq := &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "reply to service", RandomID: 3226,
	}
	groupReq.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(service.LocalID)})
	_, err = api.SendMessageForTest(s, a.ID, groupReq)
	rpcError(t, err, "MESSAGE_ID_INVALID")
}

func TestInvalidReplyTargetDoesNotPublishUpdateNudge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15553530141")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15553530142")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	c, err := s.CreateUser(ctx, "+15553530143")
	if err != nil {
		t.Fatalf("user c: %v", err)
	}
	target, _, _, _, err := s.SendMessage(ctx, a.ID, c.ID, "other dialog", 3241, 0, 0) //nolint:dogsled // only the target row and error are needed
	if err != nil {
		t.Fatalf("seed other dialog: %v", err)
	}
	listener := notificationListener(t, dsn)
	req := &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: "invalid reply", RandomID: 3242,
	}
	req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(target.LocalID)})
	_, err = api.SendMessageForTest(s, a.ID, req)
	rpcError(t, err, "MESSAGE_ID_INVALID")
	assertNoReadHistoryNotification(t, listener)
}

func TestLegacyReplyLinkIsSuppressedFromSendHistoryAndUpdates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15553530111")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15553530112")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	target, _, _, _, err := s.SendMessage(ctx, a.ID, b.ID, "target", 3211, 0, 0) //nolint:dogsled // only the target row and error are needed
	if err != nil {
		t.Fatalf("send target: %v", err)
	}
	legacy, _, _, _, err := s.SendMessage(ctx, a.ID, b.ID, "legacy reply", 3212, 0, 0) //nolint:dogsled // only the legacy row and error are needed
	if err != nil {
		t.Fatalf("send legacy row: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect raw database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close raw database: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE messages SET reply_to_msg_id = $3 WHERE owner_id = $1 AND local_id = $2`, a.ID, legacy.LocalID, target.LocalID); err != nil {
		t.Fatalf("seed legacy reply link: %v", err)
	}
	var trusted bool
	if err := conn.QueryRow(ctx, `SELECT reply_to_trusted FROM messages WHERE owner_id = $1 AND local_id = $2`, a.ID, legacy.LocalID).Scan(&trusted); err != nil {
		t.Fatalf("read seeded legacy trust: %v", err)
	}
	if trusted {
		t.Fatal("legacy reply link defaulted to trusted")
	}

	req := &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: "retry", RandomID: 3212,
	}
	response, err := api.SendMessageForTest(s, a.ID, req)
	if err != nil {
		t.Fatalf("retry legacy send: %v", err)
	}
	responseMessage := messageForIDInUpdates(t, response, legacy.LocalID)
	assertNoReplyTo(t, responseMessage, "send response")
	if err := conn.QueryRow(ctx, `SELECT reply_to_trusted FROM messages WHERE owner_id = $1 AND local_id = $2`, a.ID, legacy.LocalID).Scan(&trusted); err != nil {
		t.Fatalf("read legacy trust after retry: %v", err)
	}
	if trusted {
		t.Fatal("retry upgraded a legacy reply link to trusted")
	}

	historyEnc, err := api.GetHistoryForTest(s, a.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	history, ok := historyEnc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("history response = %T, want *tg.MessagesMessages", historyEnc)
	}
	var historyMessage *tg.Message
	for _, item := range history.Messages {
		if message, ok := item.(*tg.Message); ok && int64(message.ID) == legacy.LocalID {
			historyMessage = message
			break
		}
	}
	if historyMessage == nil {
		t.Fatalf("history omitted legacy message %d", legacy.LocalID)
	}
	assertNoReplyTo(t, historyMessage, "history")

	updates, _, _, err := api.BuildUpdatesForTest(s, a.ID, 1)
	if err != nil {
		t.Fatalf("build updates: %v", err)
	}
	var updateMessage *tg.Message
	for _, item := range updates {
		if update, ok := item.(*tg.UpdateNewMessage); ok {
			if message, ok := update.Message.(*tg.Message); ok && int64(message.ID) == legacy.LocalID {
				updateMessage = message
				break
			}
		}
	}
	if updateMessage == nil {
		t.Fatalf("updates omitted legacy message %d", legacy.LocalID)
	}
	assertNoReplyTo(t, updateMessage, "updates")
}

func messageForIDInUpdates(t *testing.T, enc bin.Encoder, id int64) *tg.Message {
	t.Helper()
	updates, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("send response = %T, want *tg.Updates", enc)
	}
	for _, item := range updates.Updates {
		if update, ok := item.(*tg.UpdateNewMessage); ok {
			if message, ok := update.Message.(*tg.Message); ok && int64(message.ID) == id {
				return message
			}
		}
	}
	t.Fatalf("send response omitted message %d", id)
	return nil
}

func assertNoReplyTo(t *testing.T, message *tg.Message, where string) {
	t.Helper()
	if replyTo, ok := message.GetReplyTo(); ok {
		t.Errorf("%s message %d has legacy ReplyTo = %+v", where, message.ID, replyTo)
	}
}

func TestGroupReplySerializesRecipientLocalTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15553530131")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15553530132")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	c, err := s.CreateUser(ctx, "+15553530133")
	if err != nil {
		t.Fatalf("user c: %v", err)
	}
	d, err := s.CreateUser(ctx, "+15553530134")
	if err != nil {
		t.Fatalf("user d: %v", err)
	}
	chat, err := s.CreateChat(ctx, a.ID, "Group replies", []int64{b.ID, c.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, d.ID, b.ID, "unrelated", 3231, 0, 0); err != nil {
		t.Fatalf("seed B's local id space: %v", err)
	}
	if _, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "root", RandomID: 3232,
	}); err != nil {
		t.Fatalf("send root: %v", err)
	}
	root, ok, err := s.MessageByRandomID(ctx, a.ID, 3232)
	if err != nil || !ok {
		t.Fatalf("load root: ok=%v err=%v", ok, err)
	}
	bHistory, err := s.History(ctx, b.ID, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil || len(bHistory) != 1 {
		t.Fatalf("B root history: len=%d err=%v", len(bHistory), err)
	}
	bTargetID := bHistory[0].LocalID
	if bTargetID == root.LocalID {
		t.Fatalf("test setup did not create owner-local id divergence: A=%d B=%d", root.LocalID, bTargetID)
	}
	beforeReplyPts, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("B state before reply: %v", err)
	}
	req := &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "reply", RandomID: 3233,
	}
	req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(root.LocalID)})
	response, err := api.SendMessageForTest(s, a.ID, req)
	if err != nil {
		t.Fatalf("send reply: %v", err)
	}
	assertReplyToMsgID(t, "group sender response", response, int(root.LocalID))

	updates, _, _, err := api.BuildUpdatesForTest(s, b.ID, beforeReplyPts.Pts)
	if err != nil {
		t.Fatalf("build B updates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("B updates = %d, want one reply", len(updates))
	}
	update, ok := updates[0].(*tg.UpdateNewMessage)
	if !ok {
		t.Fatalf("B update type = %T, want *tg.UpdateNewMessage", updates[0])
	}
	updateMessage, ok := update.Message.(*tg.Message)
	if !ok {
		t.Fatalf("B update message = %T, want *tg.Message", update.Message)
	}
	assertReplyToMsgID(t, "group recipient update", &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewMessage{Message: updateMessage}}}, int(bTargetID))

	historyEnc, err := api.GetHistoryForTest(s, b.ID, &tg.MessagesGetHistoryRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("B history: %v", err)
	}
	history, ok := historyEnc.(*tg.MessagesMessages)
	if !ok || len(history.Messages) != 2 {
		t.Fatalf("B history response = %T, len=%d; want two messages", historyEnc, len(history.Messages))
	}
	reply, ok := history.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("B history reply = %T, want *tg.Message", history.Messages[0])
	}
	if replyID, ok := replyToMessageID(reply); !ok || replyID != int(bTargetID) {
		t.Errorf("B history reply_to_msg_id = %d present=%v, want %d", replyID, ok, bTargetID)
	}
}

func replyToMessageID(message *tg.Message) (int, bool) {
	replyTo, ok := message.GetReplyTo()
	if !ok {
		return 0, false
	}
	header, ok := replyTo.(*tg.MessageReplyHeader)
	if !ok {
		return 0, false
	}
	return header.GetReplyToMsgID()
}

func TestSendMessageSamePeerReplyPassesThrough(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	a, err := s.CreateUser(ctx, "+15553530011")
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15553530012")
	if err != nil {
		t.Fatalf("user b: %v", err)
	}

	// 1:1 path: send a message, then reply with ReplyToPeerID naming the same user.
	// Asserts criterion 2: the peer field is treated as absent, not stripped —
	// the returned message carries the reply header with the sent id.
	if _, err = api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: "first", RandomID: 100,
	}); err != nil {
		t.Fatalf("1:1 initial send: %v", err)
	}
	req := &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: "reply", RandomID: 101,
	}
	replyTo := &tg.InputReplyToMessage{ReplyToMsgID: 1}
	replyTo.SetReplyToPeerID(api.InputPeerUser(a.ID, b.ID))
	req.SetReplyTo(replyTo)
	enc, err := api.SendMessageForTest(s, a.ID, req)
	if err != nil {
		t.Fatalf("1:1 same-peer reply: %v", err)
	}
	assertReplyToMsgID(t, "1:1", enc, 1)

	// Chat path: send a message, then reply with ReplyToPeerID naming the same chat.
	chat, err := s.CreateChat(ctx, a.ID, "Crew", []int64{b.ID})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, err = api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "first", RandomID: 102,
	}); err != nil {
		t.Fatalf("chat initial send: %v", err)
	}
	chatTarget, ok, err := s.MessageByRandomID(ctx, a.ID, 102)
	if err != nil || !ok {
		t.Fatalf("load chat target: ok=%v err=%v", ok, err)
	}
	req = &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: "reply", RandomID: 103,
	}
	replyTo = &tg.InputReplyToMessage{ReplyToMsgID: int(chatTarget.LocalID)}
	replyTo.SetReplyToPeerID(&tg.InputPeerChat{ChatID: chat.ID})
	req.SetReplyTo(replyTo)
	enc, err = api.SendMessageForTest(s, a.ID, req)
	if err != nil {
		t.Fatalf("chat same-peer reply: %v", err)
	}
	assertReplyToMsgID(t, "chat", enc, int(chatTarget.LocalID))

	// Channel path: post a message, then reply with ReplyToPeerID naming the same channel.
	chRes, err := api.CreateChannelForTest(s, a.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "News"})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	chID := channelIDFromCreate(t, chRes)
	postEnc, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChannel(a.ID, chID), Message: "first post", RandomID: 104,
	})
	if err != nil {
		t.Fatalf("channel initial post: %v", err)
	}
	postID := channelPostIDFromSend(t, postEnc)
	req = &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChannel(a.ID, chID), Message: "reply", RandomID: 105,
	}
	replyTo = &tg.InputReplyToMessage{ReplyToMsgID: postID}
	replyTo.SetReplyToPeerID(api.InputPeerChannel(a.ID, chID))
	req.SetReplyTo(replyTo)
	enc, err = api.SendMessageForTest(s, a.ID, req)
	if err != nil {
		t.Fatalf("channel same-peer reply: %v", err)
	}
	assertReplyToMsgID(t, "channel", enc, postID)
}

// assertReplyToMsgID checks that enc (a *tg.Updates from SendMessageForTest)
// contains a new-message update whose ReplyTo header carries wantID.
func assertReplyToMsgID(t *testing.T, path string, enc bin.Encoder, wantID int) {
	t.Helper()
	ups, ok := enc.(*tg.Updates)
	if !ok {
		t.Errorf("%s: result type = %T, want *tg.Updates", path, enc)
		return
	}
	for _, u := range ups.Updates {
		var msg *tg.Message
		switch nm := u.(type) {
		case *tg.UpdateNewMessage:
			msg, ok = nm.Message.(*tg.Message)
		case *tg.UpdateNewChannelMessage:
			msg, ok = nm.Message.(*tg.Message)
		default:
			continue
		}
		if !ok {
			continue
		}
		rt, ok := msg.GetReplyTo()
		if !ok {
			t.Errorf("%s: message has no ReplyTo header", path)
			return
		}
		hdr, ok := rt.(*tg.MessageReplyHeader)
		if !ok {
			t.Errorf("%s: ReplyTo type = %T, want *tg.MessageReplyHeader", path, rt)
			return
		}
		id, ok := hdr.GetReplyToMsgID()
		if !ok {
			t.Errorf("%s: ReplyTo header missing ReplyToMsgID", path)
			return
		}
		if id != wantID {
			t.Errorf("%s: ReplyToMsgID = %d, want %d", path, id, wantID)
		}
		return
	}
	t.Errorf("%s: no new-message update found in updates", path)
}

func TestGetMessagesRawLimitAndUnsupportedVariants(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	owner, err := s.CreateUser(context.Background(), "+15551291321")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	tooMany := make([]tg.InputMessageClass, 101)
	for i := range tooMany {
		tooMany[i] = &tg.InputMessageID{ID: 12}
	}
	_, err = api.GetMessagesForTest(s, owner.ID, &tg.MessagesGetMessagesRequest{ID: tooMany})
	if got := rpcMessage(t, err); got != "LIMIT_INVALID" {
		t.Fatalf("101 raw duplicate ids error = %s, want LIMIT_INVALID", got)
	}

	for _, unsupported := range []tg.InputMessageClass{
		&tg.InputMessagePinned{},
		&tg.InputMessageCallbackQuery{ID: 12, QueryID: 34},
	} {
		_, err = api.GetMessagesForTest(s, owner.ID, &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{
			&tg.InputMessageID{ID: 1}, unsupported,
		}})
		if got := rpcMessage(t, err); got != "INPUT_METHOD_INVALID" {
			t.Errorf("%T error = %s, want INPUT_METHOD_INVALID", unsupported, got)
		}
	}
}

func TestGetMessagesEmptyAndHiddenResultsHaveNoHydration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551291331")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551291332")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "lookup hidden rows", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	sender, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: chat.ID, FromID: creator.ID, Text: "hidden after removal", RandomID: 91331})
	if err != nil {
		t.Fatalf("send chat message: %v", err)
	}
	replySource, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: chat.ID, FromID: creator.ID, Text: "inaccessible reply source", RandomID: 91332, ReplyToMsgID: sender.LocalID})
	if err != nil {
		t.Fatalf("send reply source: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for group copy id: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()
	var memberLocalID, memberReplySourceID int64
	if err = conn.QueryRow(ctx, `SELECT local_id FROM messages WHERE owner_id = $1 AND fanout_id = $2`, member.ID, sender.FanoutID).Scan(&memberLocalID); err != nil {
		t.Fatalf("select member copy id: %v", err)
	}
	if err = conn.QueryRow(ctx, `SELECT local_id FROM messages WHERE owner_id = $1 AND fanout_id = $2`, member.ID, replySource.FanoutID).Scan(&memberReplySourceID); err != nil {
		t.Fatalf("select member reply source id: %v", err)
	}
	if _, _, _, err = s.RemoveChatUser(ctx, chat.ID, member.ID, creator.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	emptyResult, err := api.GetMessagesForTest(s, member.ID, &tg.MessagesGetMessagesRequest{})
	if err != nil {
		t.Fatalf("getMessages empty request: %v", err)
	}
	empty, ok := emptyResult.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("empty getMessages result = %T, want *tg.MessagesMessages", emptyResult)
	}
	if len(empty.Messages) != 0 || len(empty.Users) != 0 || len(empty.Chats) != 0 {
		t.Fatalf("empty request hydration = messages:%d users:%d chats:%d, want empty", len(empty.Messages), len(empty.Users), len(empty.Chats))
	}

	hiddenResult, err := api.GetMessagesForTest(s, member.ID, &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{
		&tg.InputMessageID{ID: int(memberLocalID)},
		&tg.InputMessageReplyTo{ID: int(memberReplySourceID)},
	}})
	if err != nil {
		t.Fatalf("getMessages removed member copy: %v", err)
	}
	hidden, ok := hiddenResult.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("hidden getMessages result = %T, want *tg.MessagesMessages", hiddenResult)
	}
	if len(hidden.Messages) != 2 || len(hidden.Users) != 0 || len(hidden.Chats) != 0 {
		t.Fatalf("hidden result hydration = messages:%d users:%d chats:%d, want two empties and no entities", len(hidden.Messages), len(hidden.Users), len(hidden.Chats))
	}
	for index, id := range []int{int(memberLocalID), int(memberReplySourceID)} {
		emptyMessage, ok := hidden.Messages[index].(*tg.MessageEmpty)
		if !ok || emptyMessage.ID != id {
			t.Fatalf("hidden message %d = %#v, want messageEmpty{%d}", index, hidden.Messages[index], id)
		}
	}
}

func TestGetMessagesReplyRequiresTrustedOwnerLocalTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	a, err := s.CreateUser(ctx, "+15551291341")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551291342")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	c, err := s.CreateUser(ctx, "+15551291343")
	if err != nil {
		t.Fatalf("create c: %v", err)
	}
	targetResult, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, b.ID), Message: "trusted target", RandomID: 91341,
	})
	if err != nil {
		t.Fatalf("send target: %v", err)
	}
	target := messageOf(t, targetResult)
	otherResult, err := api.SendMessageForTest(s, a.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(a.ID, c.ID), Message: "different dialog target", RandomID: 91342,
	})
	if err != nil {
		t.Fatalf("send other target: %v", err)
	}
	other := messageOf(t, otherResult)
	replyRequest := &tg.MessagesSendMessageRequest{Peer: api.InputPeerUser(a.ID, b.ID), Message: "reply source", RandomID: 91343}
	replyRequest.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: target.ID})
	replyResult, err := api.SendMessageForTest(s, a.ID, replyRequest)
	if err != nil {
		t.Fatalf("send reply source: %v", err)
	}
	replySource := messageOf(t, replyResult)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for link mutation: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()

	request := func() *tg.MessagesGetMessagesRequest {
		return &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{&tg.InputMessageReplyTo{ID: replySource.ID}}}
	}
	assertHidden := func(label string) {
		t.Helper()
		result, getErr := api.GetMessagesForTest(s, a.ID, request())
		if getErr != nil {
			t.Fatalf("%s getMessages: %v", label, getErr)
		}
		response, ok := result.(*tg.MessagesMessages)
		if !ok {
			t.Fatalf("%s getMessages result = %T, want *tg.MessagesMessages", label, result)
		}
		if len(response.Messages) != 1 || len(response.Users) != 0 || len(response.Chats) != 0 {
			t.Fatalf("%s hydration = messages:%d users:%d chats:%d, want one empty and no entities", label, len(response.Messages), len(response.Users), len(response.Chats))
		}
		empty, ok := response.Messages[0].(*tg.MessageEmpty)
		if !ok || empty.ID != replySource.ID {
			t.Fatalf("%s result = %#v, want messageEmpty{%d}", label, response.Messages[0], replySource.ID)
		}
	}

	if _, err = conn.Exec(ctx, `UPDATE messages SET reply_to_trusted = false WHERE owner_id = $1 AND local_id = $2`, a.ID, replySource.ID); err != nil {
		t.Fatalf("clear trusted link: %v", err)
	}
	assertHidden("untrusted link")
	if _, err = conn.Exec(ctx, `UPDATE messages SET reply_to_msg_id = $3, reply_to_trusted = true WHERE owner_id = $1 AND local_id = $2`, a.ID, replySource.ID, other.ID); err != nil {
		t.Fatalf("change linked target: %v", err)
	}
	assertHidden("cross-dialog target")
	if _, err = s.DeleteMessages(ctx, a.ID, []int64{int64(target.ID)}, false); err != nil {
		t.Fatalf("delete linked target: %v", err)
	}
	if _, err = conn.Exec(ctx, `UPDATE messages SET reply_to_msg_id = $3, reply_to_trusted = true WHERE owner_id = $1 AND local_id = $2`, a.ID, replySource.ID, target.ID); err != nil {
		t.Fatalf("restore link to deleted target: %v", err)
	}
	assertHidden("deleted target")
	if _, err = conn.Exec(ctx, `UPDATE messages SET reply_to_msg_id = 999999, reply_to_trusted = true WHERE owner_id = $1 AND local_id = $2`, a.ID, replySource.ID); err != nil {
		t.Fatalf("set link to missing target: %v", err)
	}
	assertHidden("missing target")
}

func TestGetMessagesRateLimitChargesInvalidInputAcrossHandlers(t *testing.T) {
	t.Parallel()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(context.Background(), "+15551291351")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	otherReplica, err := store.Open(context.Background(), dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() {
		if err := otherReplica.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
	})
	limit := store.RateLimitConfig{Limit: 1, Window: time.Minute}
	tooMany := make([]tg.InputMessageClass, 101)
	for i := range tooMany {
		tooMany[i] = &tg.InputMessageID{ID: 1}
	}
	_, err = api.GetMessagesForTestWithLimits(s, owner.ID, limit, &tg.MessagesGetMessagesRequest{ID: tooMany})
	if got := rpcMessage(t, err); got != "LIMIT_INVALID" {
		t.Fatalf("oversized request error = %s, want LIMIT_INVALID", got)
	}
	_, err = api.GetMessagesForTestWithLimits(otherReplica, owner.ID, limit, &tg.MessagesGetMessagesRequest{})
	if got := rpcMessage(t, err); got != "FLOOD_WAIT_60" {
		t.Fatalf("request on second store error = %s, want FLOOD_WAIT_60", got)
	}
}

func TestGetMessagesRejectsUnboundCallerBeforeRateCharge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15551291361")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	limit := store.RateLimitConfig{Limit: 1, Window: time.Minute}
	request := &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{&tg.InputMessageID{ID: 1}}}
	_, err = api.GetMessagesForTestWithLimits(s, 0, limit, request)
	if got := rpcMessage(t, err); got != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unbound request error = %s, want AUTH_KEY_UNREGISTERED", got)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to inspect rate counter: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM rate_limits WHERE subject_id = 0 AND surface = 'messages_get_messages'`).Scan(&count); err != nil {
		t.Fatalf("read unbound rate counter: %v", err)
	}
	if count != 0 {
		t.Fatalf("unbound requests created %d rate counter rows, want none", count)
	}
	if _, err = api.GetMessagesForTestWithLimits(s, owner.ID, limit, &tg.MessagesGetMessagesRequest{}); err != nil {
		t.Fatalf("authorized request after unbound call: %v", err)
	}
}

func TestGetMessagesNeverFallsBackAndHidesDeletedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	caller, err := s.CreateUser(ctx, "+15551291371")
	if err != nil {
		t.Fatalf("create caller: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551291372")
	if err != nil {
		t.Fatalf("create other owner: %v", err)
	}
	for i := 1; i <= 12; i++ {
		if _, _, _, _, err := s.SendMessage(ctx, other.ID, other.ID, fmt.Sprintf("other owner %d", i), int64(i), 0, 0); err != nil {
			t.Fatalf("seed other owner's local id %d: %v", i, err)
		}
	}
	result, err := api.GetMessagesForTest(s, caller.ID, &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{
		&tg.InputMessageID{ID: 12},
	}})
	if err != nil {
		t.Fatalf("get other owner's numeric id: %v", err)
	}
	response, ok := result.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("owner collision getMessages result = %T, want *tg.MessagesMessages", result)
	}
	if len(response.Messages) != 1 || len(response.Users) != 0 || len(response.Chats) != 0 {
		t.Fatalf("owner collision hydration = messages:%d users:%d chats:%d, want one empty and no entities", len(response.Messages), len(response.Users), len(response.Chats))
	}
	if empty, ok := response.Messages[0].(*tg.MessageEmpty); !ok || empty.ID != 12 {
		t.Fatalf("owner collision result = %#v, want messageEmpty{12}", response.Messages[0])
	}

	sent, err := api.SendMessageForTest(s, caller.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(caller.ID, other.ID), Message: "deleted local row", RandomID: 91371,
	})
	if err != nil {
		t.Fatalf("send row to delete: %v", err)
	}
	message := messageOf(t, sent)
	if _, err = s.DeleteMessages(ctx, caller.ID, []int64{int64(message.ID)}, false); err != nil {
		t.Fatalf("delete owner-local message: %v", err)
	}
	deletedResult, err := api.GetMessagesForTest(s, caller.ID, &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{
		&tg.InputMessageID{ID: message.ID},
	}})
	if err != nil {
		t.Fatalf("get deleted row: %v", err)
	}
	deleted, ok := deletedResult.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("deleted getMessages result = %T, want *tg.MessagesMessages", deletedResult)
	}
	if len(deleted.Messages) != 1 || len(deleted.Users) != 0 || len(deleted.Chats) != 0 {
		t.Fatalf("deleted row hydration = messages:%d users:%d chats:%d, want one empty and no entities", len(deleted.Messages), len(deleted.Users), len(deleted.Chats))
	}
	if empty, ok := deleted.Messages[0].(*tg.MessageEmpty); !ok || empty.ID != message.ID {
		t.Fatalf("deleted row result = %#v, want messageEmpty{%d}", deleted.Messages[0], message.ID)
	}
}

func TestGetMessagesUnavailableFileRendersPlainMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	a, err := s.CreateUser(ctx, "+15551291381")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551291382")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	message, file := mediaMessage(t, s, a, b, "file bytes are unavailable", true)
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("open isolated blob store: %v", err)
	}

	result, err := api.GetMessagesForTestWithBlobs(s, a.ID, &tg.MessagesGetMessagesRequest{ID: []tg.InputMessageClass{
		&tg.InputMessageID{ID: int(message.LocalID)},
	}}, blobs)
	if err != nil {
		t.Fatalf("get message with unavailable file %d: %v", file.ID, err)
	}
	response, ok := result.(*tg.MessagesMessages)
	if !ok || len(response.Messages) != 1 {
		t.Fatalf("getMessages result = %#v, want one message", result)
	}
	got, ok := response.Messages[0].(*tg.Message)
	if !ok || got.ID != int(message.LocalID) || got.Message != "file bytes are unavailable" || got.Media != nil {
		t.Fatalf("unavailable-file message = %#v, want plain message %d without media", response.Messages[0], message.LocalID)
	}
}
