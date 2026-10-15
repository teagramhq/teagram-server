package mtproto_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

// keyRingStore resolves every key it holds, bound to user, so one connection
// can switch auth keys between frames.
type keyRingStore struct {
	keys map[[8]byte]crypto.AuthKey
	user int64
}

func (s *keyRingStore) Save(context.Context, crypto.AuthKey) error { return nil }
func (s *keyRingStore) Touch(context.Context, [8]byte) error       { return nil }

func (s *keyRingStore) Get(_ context.Context, id [8]byte, _ time.Duration) (crypto.AuthKey, int64, bool, mtproto.PendingLogin, bool, error) {
	key, ok := s.keys[id]
	return key, s.user, false, mtproto.PendingLogin{}, ok, nil
}

// countingHandler counts handler invocations per request msg_id and answers
// each through reply.
type countingHandler struct {
	calls map[int64]int
	reply func(c *mtproto.Conn, req *mtproto.Request, call int) error
}

func (h *countingHandler) OnMessage(c *mtproto.Conn, req *mtproto.Request) error {
	if h.calls == nil {
		h.calls = map[int64]int{}
	}
	h.calls[req.MsgID]++
	return h.reply(c, req, h.calls[req.MsgID])
}

// serveReplayFrames serves frames on one connection and returns the server
// bodies decrypted under replyKey, or nil when replyKey is the zero key.
func serveReplayFrames(t *testing.T, keys mtproto.AuthKeyStore, h mtproto.Handler, replyKey crypto.AuthKey, frames ...[]byte) [][]byte {
	t.Helper()
	srv := mtproto.New(exchange.PrivateKey{}, 2, keys, h, nil)
	conn := &recordingFrameConn{frames: frames}
	if err := srv.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF once the script is exhausted", err)
	}
	if replyKey.Zero() {
		return nil
	}
	return conn.replies(t, replyKey)
}

// resultsFor returns every rpc_result payload addressed to msgID, in wire order.
func resultsFor(bodies [][]byte, msgID int64) [][]byte {
	var out [][]byte
	for _, body := range bodies {
		var res proto.Result
		if err := res.Decode(&bin.Buffer{Buf: body}); err != nil {
			continue
		}
		if res.RequestMessageID == msgID {
			out = append(out, res.Result)
		}
	}
	return out
}

func badMsgCodes(bodies [][]byte, msgID int64) []int {
	var out []int
	for _, body := range bodies {
		var bad mt.BadMsgNotification
		if err := bad.Decode(&bin.Buffer{Buf: body}); err != nil {
			continue
		}
		if bad.BadMsgID == msgID {
			out = append(out, bad.ErrorCode)
		}
	}
	return out
}

func encodeTL(t *testing.T, e bin.Encoder) []byte {
	t.Helper()
	var b bin.Buffer
	if err := e.Encode(&b); err != nil {
		t.Fatalf("encode %T: %v", e, err)
	}
	return b.Copy()
}

// revokingReply answers the first execution with boolTrue and any later one
// the way a handler answers on a key that execution unbound.
func revokingReply(c *mtproto.Conn, req *mtproto.Request, call int) error {
	if call == 1 {
		return c.SendResult(req, &tg.BoolTrue{})
	}
	return c.SendErr(req, tgerr.New(401, "AUTH_KEY_UNREGISTERED"))
}

// TestResentRequestIsAnsweredFromFirstExecution covers a client retransmitting
// a self-revoking RPC under the same msg_id. The first execution unbinds the
// key, so the resend arrives on an unbound key: it must get the first copy's
// boolTrue back, never run the handler a second time.
func TestResentRequestIsAnsweredFromFirstExecution(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	keys := &bindingKeyStore{key: key, users: []int64{7, 0}}
	h := &countingHandler{reply: revokingReply}

	const msgID = 1 << 32
	request := &tg.AccountResetAuthorizationRequest{Hash: 1}
	bodies := serveReplayFrames(t, keys, h, key,
		clientFrame(t, key, 42, msgID, request),
		clientFrame(t, key, 42, msgID, request),
	)

	if got := h.calls[msgID]; got != 1 {
		t.Fatalf("handler ran %d times for one msg_id, want 1", got)
	}
	results := resultsFor(bodies, msgID)
	if len(results) != 2 {
		t.Fatalf("got %d results for the msg_id, want 2 (original and resend)", len(results))
	}
	want := encodeTL(t, &tg.BoolTrue{})
	for i, res := range results {
		if string(res) != string(want) {
			t.Fatalf("result %d = %x, want boolTrue %x", i, res, want)
		}
	}
}

// TestResentRequestScopedToKeyAndSession pins that a remembered result never
// answers a request from another session or another auth key.
func TestResentRequestScopedToKeyAndSession(t *testing.T) {
	t.Parallel()
	const msgID = 1 << 32
	request := &tg.HelpGetConfigRequest{}

	t.Run("another session", func(t *testing.T) {
		t.Parallel()
		key := rebindTestKey()
		h := &countingHandler{reply: revokingReply}
		bodies := serveReplayFrames(t, &bindingKeyStore{key: key, users: []int64{7}}, h, key,
			clientFrame(t, key, 42, msgID, request),
			clientFrame(t, key, 43, msgID, request),
		)
		if got := h.calls[msgID]; got != 2 {
			t.Fatalf("handler ran %d times across two sessions, want 2", got)
		}
		results := resultsFor(bodies, msgID)
		if len(results) != 2 || string(results[0]) == string(results[1]) {
			t.Fatalf("second session was answered from the first session's result: %x", results)
		}
	})

	t.Run("another key", func(t *testing.T) {
		t.Parallel()
		keyA, keyB := rebindTestKey(), makeTestKey(0x22)
		keys := &keyRingStore{keys: map[[8]byte]crypto.AuthKey{keyA.ID: keyA, keyB.ID: keyB}, user: 7}
		h := &countingHandler{reply: revokingReply}
		serveReplayFrames(t, keys, h, crypto.AuthKey{},
			clientFrame(t, keyA, 42, msgID, request),
			clientFrame(t, keyB, 42, msgID, request),
		)
		if got := h.calls[msgID]; got != 2 {
			t.Fatalf("handler ran %d times across two keys, want 2", got)
		}
	})
}

// TestResentRequestStillRunningIsDropped covers a duplicate whose first copy
// has not produced a result: it must neither execute nor be answered.
func TestResentRequestStillRunningIsDropped(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	h := &countingHandler{reply: func(*mtproto.Conn, *mtproto.Request, int) error { return nil }}

	const msgID = 1 << 32
	request := &tg.HelpGetConfigRequest{}
	bodies := serveReplayFrames(t, &bindingKeyStore{key: key, users: []int64{7}}, h, key,
		clientFrame(t, key, 42, msgID, request),
		clientFrame(t, key, 42, msgID, request),
	)
	if got := h.calls[msgID]; got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
	if results := resultsFor(bodies, msgID); len(results) != 0 {
		t.Fatalf("unanswered request produced %d results, want 0", len(results))
	}
}

// TestDuplicateInsideContainerRunsOnce covers the same msg_id twice in one
// container: the second copy is answered with the first copy's result.
func TestDuplicateInsideContainerRunsOnce(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	keys := &bindingKeyStore{key: key, users: []int64{7}}
	h := &countingHandler{reply: revokingReply}

	const msgID = 1 << 32
	body := encodeTL(t, &tg.HelpGetConfigRequest{})
	container := &proto.MessageContainer{Messages: []proto.Message{
		{ID: msgID, SeqNo: 1, Bytes: len(body), Body: body},
		{ID: msgID, SeqNo: 1, Bytes: len(body), Body: body},
	}}
	bodies := serveReplayFrames(t, keys, h, key, clientFrame(t, key, 42, msgID+4, container))

	if got := h.calls[msgID]; got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
	results := resultsFor(bodies, msgID)
	if len(results) != 2 || string(results[0]) != string(results[1]) {
		t.Fatalf("container duplicate results = %x, want two identical", results)
	}
}

// TestServiceMessagesAreNotDeduplicated pins that only content RPCs go through
// the duplicate check: a repeated ping is answered every time.
func TestServiceMessagesAreNotDeduplicated(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	h := &countingHandler{reply: revokingReply}

	const msgID = 1 << 32
	bodies := serveReplayFrames(t, &bindingKeyStore{key: key, users: []int64{7}}, h, key,
		clientFrame(t, key, 42, msgID, &mt.PingRequest{PingID: 1}),
		clientFrame(t, key, 42, msgID, &mt.PingRequest{PingID: 1}),
	)
	pongs := 0
	for _, body := range bodies {
		var pong mt.Pong
		if pong.Decode(&bin.Buffer{Buf: body}) == nil && pong.MsgID == msgID {
			pongs++
		}
	}
	if pongs != 2 {
		t.Fatalf("got %d pongs for two pings, want 2", pongs)
	}
}

// TestEvictedRequestIsRejectedTooLow covers the bound on remembered requests:
// once the oldest entry is evicted, its resend is refused with
// bad_msg_notification 16 rather than executed again.
func TestEvictedRequestIsRejectedTooLow(t *testing.T) {
	t.Parallel()
	request := &tg.HelpGetConfigRequest{}

	t.Run("entry count", func(t *testing.T) {
		t.Parallel()
		key := rebindTestKey()
		h := &countingHandler{reply: revokingReply}
		const first = 1 << 32
		frames := make([][]byte, 0, mtproto.RPCReplayMaxEntries+2)
		for i := range mtproto.RPCReplayMaxEntries + 1 {
			frames = append(frames, clientFrame(t, key, 42, first+int64(i)*4, request))
		}
		frames = append(frames, clientFrame(t, key, 42, first, request))
		bodies := serveReplayFrames(t, &bindingKeyStore{key: key, users: []int64{7}}, h, key, frames...)

		if got := h.calls[first]; got != 1 {
			t.Fatalf("evicted request ran %d times, want 1", got)
		}
		if codes := badMsgCodes(bodies, first); len(codes) != 1 || codes[0] != 16 {
			t.Fatalf("bad_msg codes for evicted resend = %v, want [16]", codes)
		}
		if results := resultsFor(bodies, first); len(results) != 1 {
			t.Fatalf("evicted request got %d results, want 1", len(results))
		}
	})

	t.Run("total bytes", func(t *testing.T) {
		t.Parallel()
		key := rebindTestKey()
		large := strings.Repeat("x", mtproto.RPCReplayMaxEntryBytes/2)
		h := &countingHandler{reply: func(c *mtproto.Conn, req *mtproto.Request, _ int) error {
			return c.SendResult(req, &mt.RPCError{ErrorCode: 400, ErrorMessage: large})
		}}
		const first = 1 << 32
		n := mtproto.RPCReplayMaxBytes/len(large) + 2
		frames := make([][]byte, 0, n+1)
		for i := range n {
			frames = append(frames, clientFrame(t, key, 42, first+int64(i)*4, request))
		}
		frames = append(frames, clientFrame(t, key, 42, first, request))
		bodies := serveReplayFrames(t, &bindingKeyStore{key: key, users: []int64{7}}, h, key, frames...)

		if got := h.calls[first]; got != 1 {
			t.Fatalf("evicted request ran %d times, want 1", got)
		}
		if codes := badMsgCodes(bodies, first); len(codes) != 1 || codes[0] != 16 {
			t.Fatalf("bad_msg codes for evicted resend = %v, want [16]", codes)
		}
	})
}

// TestOversizedResultIsNotReplayed covers a result over the per-entry cap: it
// is remembered as answered without a body, so its duplicate is dropped.
func TestOversizedResultIsNotReplayed(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	huge := strings.Repeat("x", mtproto.RPCReplayMaxEntryBytes+1)
	h := &countingHandler{reply: func(c *mtproto.Conn, req *mtproto.Request, _ int) error {
		return c.SendResult(req, &mt.RPCError{ErrorCode: 400, ErrorMessage: huge})
	}}

	const msgID = 1 << 32
	request := &tg.HelpGetConfigRequest{}
	bodies := serveReplayFrames(t, &bindingKeyStore{key: key, users: []int64{7}}, h, key,
		clientFrame(t, key, 42, msgID, request),
		clientFrame(t, key, 42, msgID, request),
	)
	if got := h.calls[msgID]; got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
	if results := resultsFor(bodies, msgID); len(results) != 1 {
		t.Fatalf("oversized result answered %d times, want 1", len(results))
	}
}
