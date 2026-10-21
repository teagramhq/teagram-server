package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSettingSameHistoryTTLDoesNotBlockIncomingPush(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	alice, err := s.CreateUser(ctx, "+15557003701")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	bob, err := s.CreateUser(ctx, "+15557003702")
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	chat, err := s.CreateChat(ctx, alice.ID, "History TTL no-op", []int64{bob.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	const period = 86400
	if _, _, _, changed, err := s.SetUserHistoryTTL(ctx, alice.ID, bob.ID, period, store.RateLimitConfig{}); err != nil || !changed {
		t.Fatalf("set initial user history TTL: changed=%t err=%v", changed, err)
	}
	if _, _, _, changed, err := s.SetChatHistoryTTL(ctx, chat.ID, alice.ID, period, store.RateLimitConfig{}); err != nil || !changed {
		t.Fatalf("set initial chat history TTL: changed=%t err=%v", changed, err)
	}
	aliceState, err := s.State(ctx, alice.ID)
	if err != nil {
		t.Fatalf("alice state: %v", err)
	}

	registry := mtproto.NewSessionRegistry()
	updater := NewUpdater(s, 2, registry, nil, pgtest.PeerDeriver())
	key := retryTestKey(54)
	transport := &recordingNotifyTransport{}
	conn := mtproto.NewTestConn(transport, key)
	conn.SetOwner(alice.ID)
	if !registry.Add(alice.ID, conn) {
		t.Fatal("register alice connection")
	}
	t.Cleanup(func() { registry.Remove(alice.ID, conn) })
	if !conn.MarkRPCUpdate(alice.ID, conn.AuthKeyID(), aliceState.Pts) {
		t.Fatal("seed connection watermark")
	}

	_, stop, err := store.StartListener(ctx, dsn,
		updater.Deliver,
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, bool) {},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	t.Cleanup(func() { _ = stop() }) //nolint:errcheck // teardown
	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}

	h := testHandlers(s)
	setSameTTL := func(msgID int64, peer tg.InputPeerClass) {
		t.Helper()
		var body bin.Buffer
		if err := (&tg.MessagesSetHistoryTTLRequest{Peer: peer, Period: period}).Encode(&body); err != nil {
			t.Fatalf("encode setHistoryTTL request: %v", err)
		}
		req := &mtproto.Request{Ctx: ctx, UserID: alice.ID, AuthKeyID: key.ID, MsgID: msgID, Buf: &body}
		result, update, afterReply, err := h.handleSetHistoryTTLAfterReplyOnConn(conn, req)
		if err != nil {
			t.Fatalf("set same history TTL: %v", err)
		}
		if result == nil || update != nil || afterReply != nil {
			t.Fatalf("same-value result metadata = (%t, %t, %t), want result only", result != nil, update != nil, afterReply != nil)
		}
		if err := conn.SendResult(req, result); err != nil {
			t.Fatalf("send same-value TTL RPC result: %v", err)
		}
	}
	userPeer := &tg.InputPeerUser{
		UserID:     bob.ID,
		AccessHash: h.peers.Derive(alice.ID, peerhash.KindUser, bob.ID),
	}
	setSameTTL(1, userPeer)

	userSender, senderPts, userPts, userDuplicate, err := s.SendMessage(ctx, bob.ID, alice.ID, "incoming after user TTL retry", 920021, 0, 0)
	if err != nil {
		t.Fatalf("send incoming user message: %v", err)
	}
	if userDuplicate || senderPts <= 0 || userSender.LocalID <= 0 {
		t.Fatalf("incoming user send = message %d, pts %d, duplicate %t", userSender.LocalID, senderPts, userDuplicate)
	}
	if err := s.Notify(ctx, store.ChannelUpdates, strconv.FormatInt(alice.ID, 10)); err != nil {
		t.Fatalf("notify incoming user message: %v", err)
	}
	waitTransportCount(t, transport, 2)
	userHistory, err := s.History(ctx, alice.ID, store.PeerTypeUser, bob.ID, 0, 1)
	if err != nil || len(userHistory) != 1 {
		t.Fatalf("load incoming user message: messages=%d err=%v", len(userHistory), err)
	}
	frames := decodeServerFrames(t, key, transport.framesFrom(0))
	if len(frames) != 2 || frames[0].rpc == nil || frames[0].push != nil || frames[1].push == nil || frames[1].rpc != nil {
		t.Fatalf("frames after user TTL retry = %+v, want RPC result followed by incoming push", frames)
	}
	assertNewMessageFrame(t, frames[1], userHistory[0], userPts, "incoming user after TTL retry")

	setSameTTL(2, &tg.InputPeerChat{ChatID: chat.ID})
	groupSender, chatPts, chatDuplicate, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: bob.ID, Text: "incoming after group TTL retry", RandomID: 920022,
	})
	if err != nil {
		t.Fatalf("send incoming group message: %v", err)
	}
	if chatDuplicate || groupSender.LocalID <= 0 {
		t.Fatalf("incoming group send = message %d, duplicate %t", groupSender.LocalID, chatDuplicate)
	}
	if err := s.Notify(ctx, store.ChannelUpdates, strconv.FormatInt(alice.ID, 10)); err != nil {
		t.Fatalf("notify incoming group message: %v", err)
	}
	waitTransportCount(t, transport, 4)
	chatHistory, err := s.History(ctx, alice.ID, store.PeerTypeChat, chat.ID, 0, 1)
	if err != nil || len(chatHistory) != 1 {
		t.Fatalf("load incoming group message: messages=%d err=%v", len(chatHistory), err)
	}
	frames = decodeServerFrames(t, key, transport.framesFrom(0))
	if len(frames) != 4 || frames[3].push == nil || frames[3].rpc != nil {
		t.Fatalf("frames after group TTL retry = %+v, want incoming group push", frames)
	}
	assertNewMessageFrame(t, frames[3], chatHistory[0], chatPts[alice.ID], "incoming group after TTL retry")
}
