package api_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

type unhandledGatedServeKeyStore struct {
	key         crypto.AuthKey
	userID      int64
	provisional bool
}

func (s *unhandledGatedServeKeyStore) Save(context.Context, crypto.AuthKey) error { return nil }
func (s *unhandledGatedServeKeyStore) Touch(context.Context, [8]byte) error       { return nil }

func (s *unhandledGatedServeKeyStore) Get(_ context.Context, id [8]byte, _ time.Duration) (crypto.AuthKey, int64, bool, mtproto.PendingLogin, bool, error) {
	if id != s.key.ID {
		return crypto.AuthKey{}, 0, false, mtproto.PendingLogin{}, false, nil
	}
	return s.key, s.userID, s.provisional, mtproto.PendingLogin{}, true, nil
}

type unhandledGatedServeFrameConn struct {
	frames [][]byte
	next   int
	sent   [][]byte
	closed bool
}

func (c *unhandledGatedServeFrameConn) Recv(_ context.Context, b *bin.Buffer) error {
	if c.next == len(c.frames) {
		return io.EOF
	}
	b.ResetTo(slices.Clone(c.frames[c.next]))
	c.next++
	return nil
}

func (c *unhandledGatedServeFrameConn) Send(_ context.Context, b *bin.Buffer) error {
	c.sent = append(c.sent, slices.Clone(b.Buf))
	return nil
}

func (c *unhandledGatedServeFrameConn) Close() error {
	c.closed = true
	return nil
}

// TestHelpCountriesListSharedBudgetOnServeConn drives the registered method
// and the unknown-method fallback through one gotd-backed ServeConn. It catches
// a budget that is recreated per dispatch and a close verdict that fails to
// close the transport, cases that direct dispatcher calls cannot observe.
func TestHelpCountriesListSharedBudgetOnServeConn(t *testing.T) {
	t.Parallel()
	for _, state := range []struct {
		name          string
		userID        int64
		provisional   bool
		answerCode    int
		answerMessage string
	}{
		{name: "unbound", answerCode: 400, answerMessage: "INPUT_METHOD_INVALID"},
		{name: "provisional", userID: 1, provisional: true, answerCode: 401, answerMessage: "AUTH_KEY_UNREGISTERED"},
	} {
		t.Run(state.name, func(t *testing.T) {
			key := unhandledGatedServeTestKey()
			keys := &unhandledGatedServeKeyStore{key: key, userID: state.userID, provisional: state.provisional}
			blobs, err := blob.NewLocal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			handler := api.New(nil, 2, api.DefaultConfig(2, "127.0.0.1", 0), slog.New(slog.DiscardHandler), false, 100<<20, blobs, 2<<30, pgtest.PeerDeriver(), pgtest.PhotoDeriver(), config.RateLimitsConfig{}, config.RegistrationClosed)
			server := mtproto.New(exchange.PrivateKey{}, 2, keys, handler, nil)

			base := int64(proto.NewMessageID(time.Now(), proto.MessageFromClient)) &^ 3
			const frameCount = 300
			frames := make([][]byte, frameCount)
			for i := range frames {
				var call bin.Encoder = &tg.HelpGetCountriesListRequest{LangCode: "", Hash: 0}
				if i%2 == 1 {
					call = &tg.AccountRegisterDeviceRequest{}
				}
				frames[i] = unhandledGatedServeClientFrame(t, key, base+int64(i+1)<<20, call)
			}
			conn := &unhandledGatedServeFrameConn{frames: frames}

			if err := server.ServeConn(context.Background(), conn); err == nil || !strings.Contains(err.Error(), "unimplemented-method ceiling") {
				t.Fatalf("ServeConn = %v, want the unimplemented-method ceiling", err)
			}
			if conn.next != 256 {
				t.Fatalf("server read %d requests, want it to close on request 256", conn.next)
			}
			if !conn.closed {
				t.Fatal("serve loop returned at the budget ceiling without closing the transport")
			}
			if len(conn.sent) != 256 { // session-created frame plus 255 RPC replies
				t.Fatalf("server wrote %d frames, want one session-created frame and 255 RPC replies", len(conn.sent))
			}

			replies := unhandledGatedServeReplies(t, conn.sent, key)
			for call := 1; call < 256; call++ {
				result := unhandledGatedServeResult(replies, base+int64(call)<<20)
				if result == nil {
					t.Fatalf("call %d has no RPC result", call)
				}
				var rpcError mt.RPCError
				if err := rpcError.Decode(&bin.Buffer{Buf: result.Result}); err != nil {
					t.Fatalf("decode call %d error result: %v", call, err)
				}
				wantCode, wantMessage := state.answerCode, state.answerMessage
				if call > 64 {
					wantCode, wantMessage = 420, "FLOOD_WAIT_30"
				}
				if rpcError.ErrorCode != wantCode || rpcError.ErrorMessage != wantMessage {
					t.Fatalf("call %d error = %d %q, want %d %q", call, rpcError.ErrorCode, rpcError.ErrorMessage, wantCode, wantMessage)
				}
			}
			if result := unhandledGatedServeResult(replies, base+256<<20); result != nil {
				t.Fatalf("call 256 received an RPC reply after the transport closed")
			}
		})
	}
}

func unhandledGatedServeTestKey() crypto.AuthKey {
	var raw crypto.Key
	for i := range raw {
		raw[i] = byte(i)
	}
	return raw.WithID()
}

func unhandledGatedServeClientFrame(t *testing.T, key crypto.AuthKey, msgID int64, body bin.Encoder) []byte {
	t.Helper()
	var b bin.Buffer
	if err := body.Encode(&b); err != nil {
		t.Fatalf("encode client request: %v", err)
	}
	data := crypto.EncryptedMessageData{
		SessionID:              42,
		MessageID:              msgID,
		MessageDataLen:         int32(b.Len()), //nolint:gosec // test requests are small and bounded
		MessageDataWithPadding: b.Copy(),
	}
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &b); err != nil {
		t.Fatalf("encrypt client request: %v", err)
	}
	return b.Copy()
}

func unhandledGatedServeReplies(t *testing.T, frames [][]byte, key crypto.AuthKey) [][]byte {
	t.Helper()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	replies := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		message := &crypto.EncryptedMessage{}
		if err := message.DecodeWithoutCopy(&bin.Buffer{Buf: frame}); err != nil {
			t.Fatalf("decode server frame: %v", err)
		}
		decrypted, err := cipher.Decrypt(key, message)
		if err != nil {
			t.Fatalf("decrypt server frame: %v", err)
		}
		replies = append(replies, slices.Clone(decrypted.Data()))
	}
	return replies
}

func unhandledGatedServeResult(replies [][]byte, requestID int64) *proto.Result {
	for _, reply := range replies {
		var result proto.Result
		if err := result.Decode(&bin.Buffer{Buf: reply}); err == nil && result.RequestMessageID == requestID {
			return &result
		}
	}
	return nil
}
