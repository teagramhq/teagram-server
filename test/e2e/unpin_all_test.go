package e2e_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

// unpinAllFixture runs a server with two live clients, A and B, plus the
// parameters needed to bring up an extra client later.
type unpinAllFixture struct {
	t                *testing.T
	ctx              context.Context
	st               *store.Store
	port             int
	key              *rsa.PrivateKey
	codes            *multiCodeSink
	aUserID, bUserID int64
	aCmds, bCmds     chan command
	collB            *updateCollector
	errA, errB       chan error
}

func newUnpinAllFixture(t *testing.T, ctx context.Context, phoneA, phoneB string, extra ...string) *unpinAllFixture {
	t.Helper()
	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("store close: %v", cerr)
		}
	})

	const dcID = 2
	codes := newMultiCodeSink()
	ln := mustListen(t, ctx, "127.0.0.1:0")
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr type = %T", ln.Addr())
	}
	stop := bootServerWithDelivery(t, ctx, key, dcID, st, dsn, codes.Logger(), ln)
	t.Cleanup(stop)
	seedUsernameUsers(t, ctx, st, append([]string{phoneA, phoneB}, extra...)...)

	f := &unpinAllFixture{
		t: t, ctx: ctx, st: st, port: addr.Port, key: key, codes: codes,
		aCmds: make(chan command), bCmds: make(chan command), collB: newUpdateCollector(),
	}
	aID, bID := make(chan int64, 1), make(chan int64, 1)
	clientA := createClient(addr.Port, key, dcID, newUpdateCollector(), nil)
	clientB := createClient(addr.Port, key, dcID, f.collB, nil)
	f.errA, f.errB = make(chan error, 1), make(chan error, 1)
	go func() { f.errA <- runInteractive(ctx, clientA, flowFor(phoneA, codes), aID, f.aCmds) }()
	go func() { f.errB <- runInteractive(ctx, clientB, flowFor(phoneB, codes), bID, f.bCmds) }()
	f.aUserID = recvOrCtx(t, ctx, aID, "A login")
	f.bUserID = recvOrCtx(t, ctx, bID, "B login")
	return f
}

// stop ends the live clients and fails the test if either ended badly.
func (f *unpinAllFixture) stop() {
	f.t.Helper()
	close(f.aCmds)
	close(f.bCmds)
	for who, ch := range map[string]chan error{"A": f.errA, "B": f.errB} {
		if err := <-ch; err != nil && !errors.Is(err, context.Canceled) {
			f.t.Errorf("client %s: %v", who, err)
		}
	}
}

func (f *unpinAllFixture) exec(cmds chan command, fn func(ctx context.Context, c *tg.Client) error) error {
	d := make(chan error, 1)
	select {
	case cmds <- command{fn: fn, done: d}:
	case <-f.ctx.Done():
		f.t.Fatalf("command enqueue timeout: %v", f.ctx.Err())
	}
	return <-d
}

// session runs fn on a short-lived client for phone that persists its auth key
// in sess, so the same account can go offline and come back.
func (f *unpinAllFixture) session(phone string, sess *session.StorageMemory, fn func(ctx context.Context, c *telegram.Client) error) error {
	client := createClient(f.port, f.key, 2, newUpdateCollector(), sess)
	return client.Run(f.ctx, func(ctx context.Context) error {
		if err := client.Auth().IfNecessary(ctx, flowFor(phone, f.codes)); err != nil {
			return err
		}
		return fn(ctx, client)
	})
}

func (f *unpinAllFixture) unpinAll(cmds chan command, req *tg.MessagesUnpinAllMessagesRequest) (*tg.MessagesAffectedHistory, error) {
	var got *tg.MessagesAffectedHistory
	err := f.exec(cmds, func(ctx context.Context, c *tg.Client) error {
		var err error
		got, err = c.MessagesUnpinAllMessages(ctx, req)
		return err
	})
	return got, err
}

func wantRPCError(t *testing.T, err error, want, what string) {
	t.Helper()
	var rpcErr *tgerr.Error
	if !errors.As(err, &rpcErr) || rpcErr.Type != want {
		t.Fatalf("%s error = %v, want %s", what, err, want)
	}
}

func chatPinnedID(ctx context.Context, c *tg.Client, chatID int64) (int, bool, error) {
	res, err := c.MessagesGetFullChat(ctx, chatID)
	if err != nil {
		return 0, false, err
	}
	full, ok := res.FullChat.(*tg.ChatFull)
	if !ok {
		return 0, false, errors.New("getFullChat: not a *tg.ChatFull")
	}
	id, ok := full.GetPinnedMsgID()
	return id, ok && id != 0, nil
}

func channelPinnedID(ctx context.Context, c *tg.Client, channel *tg.InputChannel) (int, bool, error) {
	res, err := c.ChannelsGetFullChannel(ctx, channel)
	if err != nil {
		return 0, false, err
	}
	full, ok := res.FullChat.(*tg.ChannelFull)
	if !ok {
		return 0, false, errors.New("getFullChannel: not a *tg.ChannelFull")
	}
	id, ok := full.GetPinnedMsgID()
	return id, ok && id != 0, nil
}

// TestUnpinAllMessagesChat proves messages.unpinAllMessages in a basic group:
// the creator clears the pin with the unpin push, a member is refused, a
// repeat call changes and pushes nothing, topic and monoforum requests are
// refused, and a member that was offline sees no pin when it comes back.
func TestUnpinAllMessagesChat(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const phoneA, phoneB, phoneC = "+15551868001", "+15551868002", "+15551868003"
	f := newUnpinAllFixture(t, ctx, phoneA, phoneB, phoneC)

	// C signs in once and goes offline for the rest of the test.
	sessC := &session.StorageMemory{}
	var cUserID int64
	if err := f.session(phoneC, sessC, func(ctx context.Context, c *telegram.Client) error {
		self, err := c.Self(ctx)
		if err != nil {
			return err
		}
		cUserID = self.ID
		return nil
	}); err != nil {
		t.Fatalf("C sign in: %v", err)
	}

	var chatID int64
	var msgID int
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		inv, err := c.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Unpin All",
			Users: []tg.InputUserClass{inputUser(f.aUserID, f.bUserID), inputUser(f.aUserID, cUserID)},
		})
		if err != nil {
			return err
		}
		ups, ok := inv.Updates.(*tg.Updates)
		if !ok || len(ups.Chats) == 0 {
			return errors.New("createChat: unexpected updates")
		}
		chat, ok := ups.Chats[0].(*tg.Chat)
		if !ok {
			return errors.New("createChat: not a *tg.Chat")
		}
		chatID = chat.ID
		res, err := c.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: "unpin all me", RandomID: 1868001,
		})
		if err != nil {
			return err
		}
		sent, ok := res.(*tg.Updates)
		if !ok {
			return errors.New("send: unexpected result")
		}
		for _, u := range sent.Updates {
			if nm, ok := u.(*tg.UpdateNewMessage); ok {
				if m, ok := nm.Message.(*tg.Message); ok {
					msgID = m.ID
				}
			}
		}
		if msgID == 0 {
			return errors.New("send: no message id")
		}
		_, err = c.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: msgID,
		})
		return err
	}); err != nil {
		t.Fatalf("A create, send and pin: %v", err)
	}
	pin := recvOrCtx(t, ctx, f.collB.pinnedMsg, "B pin push")
	if !pin.Pinned || len(pin.Messages) != 1 {
		t.Fatalf("B pin push = {pinned:%t messages:%v}, want true/[B copy]", pin.Pinned, pin.Messages)
	}
	// A live push carries the pts of the event B's difference replays.
	assertPushedEvent := func(what string, pushed *tg.UpdatePinnedMessages, want store.EventType) {
		t.Helper()
		if pushed.PtsCount != 1 {
			t.Fatalf("B %s push pts_count = %d, want 1", what, pushed.PtsCount)
		}
		events, err := f.st.EventsSince(ctx, f.bUserID, pushed.Pts-1)
		if err != nil {
			t.Fatalf("B events: %v", err)
		}
		wantEvent := store.Event{Pts: pushed.Pts, Type: want, LocalID: int64(pushed.Messages[0])}
		if len(events) != 1 || events[0] != wantEvent {
			t.Fatalf("B events from pushed %s pts %d = %+v, want only %+v", what, pushed.Pts, events, wantEvent)
		}
	}
	assertPushedEvent("pin", pin, store.EventPin)

	assertPinned := func(want bool) {
		t.Helper()
		if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
			id, pinned, err := chatPinnedID(ctx, c, chatID)
			if err != nil {
				return err
			}
			if pinned != want || (want && id != msgID) {
				return fmt.Errorf("pinned id = %d/%t, want message %d", id, pinned, msgID)
			}
			return nil
		}); err != nil {
			t.Fatalf("A getFullChat pinned=%t: %v", want, err)
		}
	}

	// Refused calls change nothing.
	if _, _, err := f.st.SetChatDefaultBannedRights(ctx, chatID, f.aUserID, []string{"pin_messages"}); err != nil {
		t.Fatal(err)
	}
	_, err := f.unpinAll(f.bCmds, &tg.MessagesUnpinAllMessagesRequest{Peer: &tg.InputPeerChat{ChatID: chatID}})
	wantRPCError(t, err, "CHAT_ADMIN_REQUIRED", "B unpinAll as member")
	topic := &tg.MessagesUnpinAllMessagesRequest{Peer: &tg.InputPeerChat{ChatID: chatID}}
	topic.SetTopMsgID(msgID)
	_, err = f.unpinAll(f.aCmds, topic)
	wantRPCError(t, err, "MESSAGE_ID_INVALID", "A unpinAll with top_msg_id")
	saved := &tg.MessagesUnpinAllMessagesRequest{Peer: &tg.InputPeerChat{ChatID: chatID}}
	saved.SetSavedPeerID(&tg.InputPeerSelf{})
	_, err = f.unpinAll(f.aCmds, saved)
	wantRPCError(t, err, "PEER_ID_INVALID", "A unpinAll with saved_peer_id")
	assertPinned(true)

	// Private peers get the answer updatePinnedMessage gives them.
	privatePeer := &tg.InputPeerUser{UserID: f.bUserID, AccessHash: inputUser(f.aUserID, f.bUserID).AccessHash}
	pinErr := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{Peer: privatePeer, Unpin: true})
		return err
	})
	var pinRPC *tgerr.Error
	if !errors.As(pinErr, &pinRPC) {
		t.Fatalf("private updatePinnedMessage error = %v, want RPC error", pinErr)
	}
	_, err = f.unpinAll(f.aCmds, &tg.MessagesUnpinAllMessagesRequest{Peer: privatePeer})
	wantRPCError(t, err, pinRPC.Type, "A unpinAll on private peer")

	// The creator clears the pin.
	statePts := func() int {
		t.Helper()
		state, err := f.st.StateWithoutUnread(ctx, f.aUserID)
		if err != nil {
			t.Fatalf("A state: %v", err)
		}
		return state.Pts
	}
	before := statePts()
	affected, err := f.unpinAll(f.aCmds, &tg.MessagesUnpinAllMessagesRequest{Peer: &tg.InputPeerChat{ChatID: chatID}})
	if err != nil {
		t.Fatalf("A unpinAll: %v", err)
	}
	if affected.Pts != before+1 || affected.PtsCount != 1 || affected.Offset != 0 {
		t.Fatalf("A unpinAll = %+v, want pts %d, pts_count 1, offset 0", affected, before+1)
	}
	if after := statePts(); after != before+1 {
		t.Fatalf("A pts after unpinAll = %d, want %d", after, before+1)
	}
	before++
	unpin := recvOrCtx(t, ctx, f.collB.pinnedMsg, "B unpin push")
	if unpin.Pinned || len(unpin.Messages) != 1 || unpin.Messages[0] != pin.Messages[0] || unpin.PtsCount != 1 {
		t.Fatalf("B unpin push = %+v, want false/[B copy %d] with pts_count 1", unpin, pin.Messages[0])
	}
	if unpin.Pts != pin.Pts+1 {
		t.Fatalf("B unpin push pts = %d, want pin pts %d + 1", unpin.Pts, pin.Pts)
	}
	assertPushedEvent("unpin", unpin, store.EventUnpin)
	if peer, ok := unpin.Peer.(*tg.PeerChat); !ok || peer.ChatID != chatID {
		t.Fatalf("B unpin push peer = %#v, want chat %d", unpin.Peer, chatID)
	}
	assertPinned(false)

	// Nothing pinned: success, no change, no push. The next push B sees is the
	// re-pin below, so no unpin push slipped in between.
	again, err := f.unpinAll(f.aCmds, &tg.MessagesUnpinAllMessagesRequest{Peer: &tg.InputPeerChat{ChatID: chatID}})
	if err != nil {
		t.Fatalf("A unpinAll with nothing pinned: %v", err)
	}
	if again.Pts != before || again.PtsCount != 0 {
		t.Fatalf("A repeat unpinAll = %+v, want pts %d, pts_count 0", again, before)
	}
	assertPinned(false)

	// C comes back and sees no pin.
	if err := f.session(phoneC, sessC, func(ctx context.Context, c *telegram.Client) error {
		if _, err := c.API().UpdatesGetState(ctx); err != nil {
			return err
		}
		_, pinned, err := chatPinnedID(ctx, c.API(), chatID)
		if err != nil {
			return err
		}
		if pinned {
			return errors.New("offline member still sees a pin")
		}
		return nil
	}); err != nil {
		t.Fatalf("C reconnect: %v", err)
	}

	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: msgID,
		})
		return err
	}); err != nil {
		t.Fatalf("A re-pin: %v", err)
	}
	if next := recvOrCtx(t, ctx, f.collB.pinnedMsg, "B re-pin push"); !next.Pinned {
		t.Fatal("B next push after no-op unpinAll is an unpin, want the re-pin")
	}
	f.stop()
}

// TestUnpinAllMessagesChannel proves messages.unpinAllMessages in a channel:
// a non-admin is refused, the admin clears the pin with the unpin push and the
// channel pts, and a repeat call changes and pushes nothing.
func TestUnpinAllMessagesChannel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const phoneA, phoneB = "+15551868011", "+15551868012"
	f := newUnpinAllFixture(t, ctx, phoneA, phoneB)

	var channelID int64
	var inviteLink string
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		res, err := c.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
			Title: "Unpin All Channel", Megagroup: true,
		})
		if err != nil {
			return err
		}
		ups, ok := res.(*tg.Updates)
		if !ok || len(ups.Chats) == 0 {
			return errors.New("createChannel: unexpected result")
		}
		ch, ok := ups.Chats[0].(*tg.Channel)
		if !ok {
			return errors.New("createChannel: not a *tg.Channel")
		}
		channelID = ch.ID
		inv, err := c.MessagesExportChatInvite(ctx, &tg.MessagesExportChatInviteRequest{Peer: peerChannel(f.aUserID, channelID)})
		if err != nil {
			return err
		}
		exported, ok := inv.(*tg.ChatInviteExported)
		if !ok {
			return errors.New("exportChatInvite: unexpected type")
		}
		inviteLink = exported.Link
		return nil
	}); err != nil {
		t.Fatalf("A create channel: %v", err)
	}
	if err := f.exec(f.bCmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesImportChatInvite(ctx, inviteHash(inviteLink))
		return err
	}); err != nil {
		t.Fatalf("B join: %v", err)
	}

	var postID int
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		res, err := c.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerChannel(f.aUserID, channelID), Message: "unpin all post", RandomID: 1868011,
		})
		if err != nil {
			return err
		}
		ups, ok := res.(*tg.Updates)
		if !ok {
			return errors.New("send: unexpected result")
		}
		for _, u := range ups.Updates {
			if nm, ok := u.(*tg.UpdateNewChannelMessage); ok {
				if m, ok := nm.Message.(*tg.Message); ok {
					postID = m.ID
				}
			}
		}
		if postID == 0 {
			return errors.New("send: no post id")
		}
		return nil
	}); err != nil {
		t.Fatalf("A post: %v", err)
	}
	// A channel push replays the newest event, so B takes the post's push
	// before the pin commits; otherwise it may carry the pin a second time.
	recvOrCtx(t, ctx, f.collB.newChannelMsg, "B channel post")
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: peerChannel(f.aUserID, channelID), ID: postID,
		})
		return err
	}); err != nil {
		t.Fatalf("A pin: %v", err)
	}
	if pin := recvOrCtx(t, ctx, f.collB.pinnedChanMsg, "B pin push"); !pin.Pinned {
		t.Fatal("B pin push Pinned = false, want true")
	}

	// Verify the stored pin and its full-channel rendering for both members.
	assertPinned := func(want bool) {
		t.Helper()
		id, err := f.st.ChannelPinnedMessage(ctx, channelID)
		if err != nil {
			t.Fatalf("channel pinned message: %v", err)
		}
		if (id != nil) != want || (want && int(*id) != postID) {
			t.Fatalf("channel pinned message = %v, want pinned=%t post %d", id, want, postID)
		}
		for _, viewer := range []struct {
			cmds chan command
			id   int64
		}{{f.aCmds, f.aUserID}, {f.bCmds, f.bUserID}} {
			if err := f.exec(viewer.cmds, func(ctx context.Context, c *tg.Client) error {
				id, pinned, err := channelPinnedID(ctx, c, inputChannel(viewer.id, channelID))
				if err != nil {
					return err
				}
				if want && (!pinned || id != postID) {
					return fmt.Errorf("pinned id = %d/%t, want %d", id, pinned, postID)
				}
				if !want && pinned {
					return fmt.Errorf("pinned id = %d, want none", id)
				}
				return nil
			}); err != nil {
				t.Fatalf("getFullChannel for %d: %v", viewer.id, err)
			}
		}
	}

	_, err := f.unpinAll(f.bCmds, &tg.MessagesUnpinAllMessagesRequest{Peer: peerChannel(f.bUserID, channelID)})
	wantRPCError(t, err, "CHAT_ADMIN_REQUIRED", "B unpinAll as non-admin")
	topic := &tg.MessagesUnpinAllMessagesRequest{Peer: peerChannel(f.aUserID, channelID)}
	topic.SetTopMsgID(postID)
	_, err = f.unpinAll(f.aCmds, topic)
	wantRPCError(t, err, "MESSAGE_ID_INVALID", "A unpinAll with top_msg_id")
	assertPinned(true)

	before, err := f.st.ChannelState(ctx, channelID)
	if err != nil {
		t.Fatalf("channel pts: %v", err)
	}
	affected, err := f.unpinAll(f.aCmds, &tg.MessagesUnpinAllMessagesRequest{Peer: peerChannel(f.aUserID, channelID)})
	if err != nil {
		t.Fatalf("A unpinAll: %v", err)
	}
	if affected.Pts != before+1 || affected.PtsCount != 1 || affected.Offset != 0 {
		t.Fatalf("A unpinAll = %+v, want pts %d, pts_count 1, offset 0", affected, before+1)
	}
	unpin := recvOrCtx(t, ctx, f.collB.pinnedChanMsg, "B unpin push")
	if unpin.Pinned || len(unpin.Messages) != 1 || unpin.Messages[0] != postID {
		t.Fatalf("B unpin push = {pinned:%t messages:%v}, want false/[%d]", unpin.Pinned, unpin.Messages, postID)
	}
	if unpin.ChannelID != channelID || unpin.Pts != before+1 || unpin.PtsCount != 1 {
		t.Fatalf("B unpin push = %+v, want channel %d at pts %d, pts_count 1", unpin, channelID, before+1)
	}
	assertPinned(false)

	again, err := f.unpinAll(f.aCmds, &tg.MessagesUnpinAllMessagesRequest{Peer: peerChannel(f.aUserID, channelID)})
	if err != nil {
		t.Fatalf("A unpinAll with nothing pinned: %v", err)
	}
	if again.Pts != before+1 || again.PtsCount != 0 {
		t.Fatalf("A repeat unpinAll = %+v, want pts %d, pts_count 0", again, before+1)
	}
	if after, err := f.st.ChannelState(ctx, channelID); err != nil || after != before+1 {
		t.Fatalf("channel pts after repeat unpinAll = %d (%v), want unchanged %d", after, err, before+1)
	}
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: peerChannel(f.aUserID, channelID), ID: postID,
		})
		return err
	}); err != nil {
		t.Fatalf("A re-pin: %v", err)
	}
	if next := recvOrCtx(t, ctx, f.collB.pinnedChanMsg, "B re-pin push"); !next.Pinned {
		t.Fatal("B next push after no-op unpinAll is an unpin, want the re-pin")
	}
	f.stop()
}
