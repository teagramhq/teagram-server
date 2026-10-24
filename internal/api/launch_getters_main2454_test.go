package api_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestMAIN2474CollectibleEmojiStatusesHashThroughDispatcher(t *testing.T) {
	t.Parallel()
	h := main2454Dispatcher()

	for _, test := range []struct {
		name            string
		hash            int64
		wantNotModified bool
	}{
		{name: "matching empty-list hash", hash: 0, wantNotModified: true},
		{name: "different hash", hash: 123456789},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := dispatchSettings(t, h, settingsHandler{
				name: "account.getCollectibleEmojiStatuses",
				request: func() bin.Encoder {
					return &tg.AccountGetCollectibleEmojiStatusesRequest{Hash: test.hash}
				},
				response: func() bin.Decoder { return &tg.AccountEmojiStatusesBox{} },
			}, 1, false)
			response := &tg.AccountEmojiStatusesBox{}
			if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
				t.Fatalf("decode collectible emoji statuses: %v", err)
			}
			if test.wantNotModified {
				if _, ok := response.EmojiStatuses.(*tg.AccountEmojiStatusesNotModified); !ok {
					t.Fatalf("collectible emoji statuses = %T, want not modified", response.EmojiStatuses)
				}
				return
			}
			assertEmptyEmojiStatuses(t, response)
		})
	}
}

func TestMAIN2454CollectibleEmojiStatusesRequiresBoundUser(t *testing.T) {
	t.Parallel()
	body := dispatchSettings(t, main2454Dispatcher(), settingsHandler{
		name: "account.getCollectibleEmojiStatuses",
		request: func() bin.Encoder {
			return &tg.AccountGetCollectibleEmojiStatusesRequest{Hash: 0}
		},
	}, 0, false)

	rpc := &mt.RPCError{}
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode unauthenticated reply: %v", err)
	}
	if rpc.ErrorCode != 401 || rpc.ErrorMessage != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unauthenticated reply = %d %q, want 401 AUTH_KEY_UNREGISTERED", rpc.ErrorCode, rpc.ErrorMessage)
	}
}

func TestMAIN2454CollectibleEmojiStatusesRejectsMalformedRequest(t *testing.T) {
	t.Parallel()
	request := &bin.Buffer{}
	request.PutID(tg.AccountGetCollectibleEmojiStatusesRequestTypeID)
	body := dispatchMAIN2454Body(t, main2454Dispatcher(), 1, request)

	rpc := &mt.RPCError{}
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode malformed-request reply: %v", err)
	}
	if rpc.ErrorCode != 400 || rpc.ErrorMessage != "INPUT_METHOD_INVALID" {
		t.Fatalf("malformed-request reply = %d %q, want 400 INPUT_METHOD_INVALID", rpc.ErrorCode, rpc.ErrorMessage)
	}
}

func TestMAIN2454CollectibleEmojiStatusesDoNotChargeUnimplementedBudget(t *testing.T) {
	t.Parallel()
	h := main2454Dispatcher()
	key := testKey()
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)
	conn.SetClock(&testClock{now: time.Now()})

	const firstMsgID = int64(1 << 32)
	for i := range 255 {
		var body bin.Buffer
		if err := (&tg.AccountRegisterDeviceRequest{}).Encode(&body); err != nil {
			t.Fatalf("encode unimplemented request %d: %v", i+1, err)
		}
		err := h.OnMessage(conn, &mtproto.Request{
			AuthKeyID: key.ID,
			UserID:    1,
			MsgID:     firstMsgID + int64(i+1)*4,
			Buf:       &body,
			Ctx:       context.Background(),
		})
		rpc := mustRPCError(t, err)
		wantCode, wantMessage := 400, "INPUT_METHOD_INVALID"
		if i >= 64 {
			wantCode, wantMessage = 420, "FLOOD_WAIT_30"
		}
		if rpc.Code != wantCode || rpc.Message != wantMessage {
			t.Fatalf("unimplemented request %d = %d %s, want %d %s", i+1, rpc.Code, rpc.Message, wantCode, wantMessage)
		}
	}

	var body bin.Buffer
	if err := (&tg.AccountGetCollectibleEmojiStatusesRequest{Hash: 123456789}).Encode(&body); err != nil {
		t.Fatalf("encode collectible emoji statuses request: %v", err)
	}
	const finalMsgID = firstMsgID + 256*4
	if err := h.OnMessage(conn, &mtproto.Request{
		AuthKeyID: key.ID,
		UserID:    1,
		MsgID:     finalMsgID,
		Buf:       &body,
		Ctx:       context.Background(),
	}); err != nil {
		t.Fatalf("dispatch collectible emoji statuses after 255 unimplemented calls: %v", err)
	}

	response := &tg.AccountEmojiStatusesBox{}
	if err := response.Decode(&bin.Buffer{Buf: transport.result(t, key, finalMsgID)}); err != nil {
		t.Fatalf("decode collectible emoji statuses after unimplemented calls: %v", err)
	}
	assertEmptyEmojiStatuses(t, response)
}

func main2454Dispatcher() mtproto.Handler {
	return api.New(
		nil,
		2,
		&tg.Config{},
		slog.New(slog.DiscardHandler),
		false,
		1,
		nil,
		1,
		pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{},
		config.RegistrationInvite,
	)
}

func dispatchMAIN2454Body(t *testing.T, h mtproto.Handler, userID int64, request *bin.Buffer) []byte {
	t.Helper()
	key := testKey()
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)
	const msgID = int64(1 << 32)
	if err := h.OnMessage(conn, &mtproto.Request{
		AuthKeyID: key.ID,
		UserID:    userID,
		MsgID:     msgID,
		Buf:       request,
		Ctx:       context.Background(),
	}); err != nil {
		t.Fatalf("dispatch malformed request: %v", err)
	}
	return transport.result(t, key, msgID)
}
