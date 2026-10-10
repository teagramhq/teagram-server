package mtproto_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
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

// recordingFrameConn serves scripted frames and records every encrypted
// message the server writes back, so a test can decrypt them and assert on the
// actual replies that reached the wire.
type recordingFrameConn struct {
	frames [][]byte
	i      int

	mu   sync.Mutex
	sent [][]byte
}

func (c *recordingFrameConn) Recv(_ context.Context, b *bin.Buffer) error {
	if c.i >= len(c.frames) {
		return io.EOF
	}
	b.ResetTo(slices.Clone(c.frames[c.i]))
	c.i++
	return nil
}

func (c *recordingFrameConn) Send(_ context.Context, b *bin.Buffer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, slices.Clone(b.Buf))
	return nil
}

func (c *recordingFrameConn) Close() error { return nil }

// replies decrypts every recorded frame under key and returns the plaintext
// message bodies in wire order.
func (c *recordingFrameConn) replies(t *testing.T, key crypto.AuthKey) [][]byte {
	t.Helper()
	c.mu.Lock()
	frames := slices.Clone(c.sent)
	c.mu.Unlock()

	// A client-side cipher, not the server's one: the two sides of an MTProto
	// session encrypt toward each other, so what the server wrote is what a
	// client decrypts.
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		m := &crypto.EncryptedMessage{}
		if err := m.DecodeWithoutCopy(&bin.Buffer{Buf: f}); err != nil {
			t.Fatalf("decode server frame: %v", err)
		}
		msg, err := cipher.Decrypt(key, m)
		if err != nil {
			t.Fatalf("decrypt server frame: %v", err)
		}
		out = append(out, slices.Clone(msg.Data()))
	}
	return out
}

// findResult scans decrypted bodies for the RPC result addressed to msgID and
// returns its raw payload.
func findResult(t *testing.T, bodies [][]byte, msgID int64) []byte {
	t.Helper()
	for _, body := range bodies {
		var res proto.Result
		if err := res.Decode(&bin.Buffer{Buf: body}); err != nil {
			continue
		}
		if res.RequestMessageID != msgID {
			continue
		}
		return res.Result
	}
	t.Fatalf("no RPC result for msg id %d in %d replies", msgID, len(bodies))
	return nil
}

// TestRPCDeadlineSurvivingHandlerReplies pins what a handler that finishes
// after its own deadline owes the client. The deadline abandons the request,
// never the reply: a handler holding a completed result must see it delivered
// as a result — not silently swapped for INTERNAL, which would tell the client
// committed work failed — and a tgerr it returns is answered as itself, since
// it already carries the method's error surface. In both cases the connection
// stays up and the next frame on it is served.
func TestRPCDeadlineSurvivingHandlerReplies(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()

	// run drives one connection through a timed-out RPC followed by a ping,
	// and hands back every decrypted reply in wire order.
	run := func(t *testing.T, h mtproto.Handler) [][]byte {
		t.Helper()
		ks := &statusKeyStore{key: key, users: []int64{0}}
		srv := mtproto.New(exchange.PrivateKey{}, 2, ks, h, nil)
		if err := srv.SetRPCDeadline(150 * time.Millisecond); err != nil {
			t.Fatalf("set rpc deadline: %v", err)
		}
		conn := &recordingFrameConn{frames: [][]byte{
			statusClientFrame(t, key, 42, 1<<32, &tg.AccountRegisterDeviceRequest{}),
			statusClientFrame(t, key, 42, 2<<32, &mt.PingRequest{PingID: 9}),
		}}
		// EOF, not an error: the connection survived the abandoned request.
		if err := srv.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
		return conn.replies(t, key)
	}

	// pongFor finds the pong answering the trailing ping — the proof the serve
	// loop kept reading after the deadline fired.
	pongFor := func(t *testing.T, replies [][]byte) *mt.Pong {
		t.Helper()
		for _, body := range replies {
			p := &mt.Pong{}
			if err := p.Decode(&bin.Buffer{Buf: body}); err == nil && p.MsgID == 2<<32 {
				return p
			}
		}
		t.Fatalf("no pong for the post-deadline frame in %d replies", len(replies))
		return nil
	}

	t.Run("result", func(t *testing.T) {
		h := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
			<-req.Ctx.Done()
			return c.SendResult(req, &tg.BoolTrue{})
		})
		replies := run(t, h)
		res := &tg.BoolTrue{}
		if err := res.Decode(&bin.Buffer{Buf: findResult(t, replies, 1<<32)}); err != nil {
			t.Fatalf("reply past the deadline is not the handler's result: %v", err)
		}
		pongFor(t, replies)
	})

	t.Run("tgerr", func(t *testing.T) {
		h := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
			<-req.Ctx.Done()
			return tgerr.New(400, "PEER_ID_INVALID")
		})
		replies := run(t, h)
		e := &mt.RPCError{}
		if err := e.Decode(&bin.Buffer{Buf: findResult(t, replies, 1<<32)}); err != nil {
			t.Fatalf("decode rpc error past the deadline: %v", err)
		}
		if e.ErrorCode != 400 || e.ErrorMessage != "PEER_ID_INVALID" {
			t.Fatalf("rpc error = %d %q, want the handler's own 400 PEER_ID_INVALID",
				e.ErrorCode, e.ErrorMessage)
		}
		pongFor(t, replies)
	})
}

// rawBodyFrame encrypts an already-encoded message body the way a real client
// puts it on the wire, for bodies like a MessageContainer that the test builds
// byte-for-byte rather than through bin.Encoder.
func rawBodyFrame(t *testing.T, key crypto.AuthKey, sessionID, msgID int64, body bin.Buffer) []byte {
	t.Helper()
	data := crypto.EncryptedMessageData{
		SessionID:              sessionID,
		MessageID:              msgID,
		MessageDataLen:         int32(body.Len()), //nolint:gosec // test data, far below MaxInt32
		MessageDataWithPadding: body.Copy(),
	}
	var b bin.Buffer
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &b); err != nil {
		t.Fatalf("encrypt frame: %v", err)
	}
	return b.Copy()
}

// TestRPCDeadlineSharedFromContainerAdmission pins the deadline to when a
// container is admitted, not when each of its serial entries reaches the
// handler. Later entries cannot get a fresh budget merely by waiting behind an
// earlier RPC in the same frame.
func TestRPCDeadlineSharedFromContainerAdmission(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	ks := &statusKeyStore{key: key, users: []int64{0}}

	const deadline = 150 * time.Millisecond
	var mu sync.Mutex
	var budgets []time.Duration
	var expiredAtEntry []bool
	var completionBudgets []time.Duration
	var completionExpiredAtEntry []bool
	var requestDeadlines []time.Time
	var completionDeadlines []time.Time
	h := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		if dl, ok := req.Ctx.Deadline(); ok {
			mu.Lock()
			budgets = append(budgets, time.Until(dl))
			expiredAtEntry = append(expiredAtEntry, req.Ctx.Err() != nil)
			requestDeadlines = append(requestDeadlines, dl)
			index := len(budgets)
			if req.CompletionCtx == nil {
				t.Error("request has no completion context")
			} else {
				if completionDL, completionOK := req.CompletionCtx.Deadline(); completionOK {
					completionBudgets = append(completionBudgets, time.Until(completionDL))
					completionExpiredAtEntry = append(completionExpiredAtEntry, req.CompletionCtx.Err() != nil)
					completionDeadlines = append(completionDeadlines, completionDL)
				} else {
					t.Error("completion context carries no deadline")
				}
			}
			mu.Unlock()
			if index == 1 {
				<-req.Ctx.Done()
			}
		} else {
			t.Error("request context carries no deadline")
		}
		return req.Ctx.Err()
	})
	srv := mtproto.New(exchange.PrivateKey{}, 2, ks, h, nil)
	if err := srv.SetRPCDeadline(deadline); err != nil {
		t.Fatalf("set rpc deadline: %v", err)
	}

	msgs := make([]proto.Message, 2)
	for i := range msgs {
		var b bin.Buffer
		if err := (&tg.AccountRegisterDeviceRequest{}).Encode(&b); err != nil {
			t.Fatalf("encode body: %v", err)
		}
		msgs[i] = proto.Message{
			ID:    1<<32 | int64(i),
			SeqNo: i + 1,
			Bytes: b.Len(),
			Body:  b.Copy(),
		}
	}
	var cbin bin.Buffer
	if err := (&proto.MessageContainer{Messages: msgs}).Encode(&cbin); err != nil {
		t.Fatalf("encode container: %v", err)
	}
	conn := &recordingFrameConn{frames: [][]byte{
		rawBodyFrame(t, key, 42, 7<<32, cbin),
	}}
	if err := srv.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want EOF", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 2 || len(completionBudgets) != 2 || len(requestDeadlines) != 2 || len(completionDeadlines) != 2 {
		t.Fatalf("handler saw %d request and %d completion deadlines, want both container entries dispatched", len(budgets), len(completionBudgets))
	}
	if budgets[0] <= 0 || completionBudgets[0] <= 0 || !expiredAtEntry[1] || !completionExpiredAtEntry[1] {
		t.Fatalf("request budgets = %v (expired %v), completion budgets = %v (expired %v); want both second-entry contexts to share the elapsed admission deadline",
			budgets, expiredAtEntry, completionBudgets, completionExpiredAtEntry)
	}
	for i := range requestDeadlines {
		if !requestDeadlines[i].Equal(completionDeadlines[i]) {
			t.Fatalf("request deadline %d = %s, completion deadline = %s; want the same admission deadline", i, requestDeadlines[i], completionDeadlines[i])
		}
	}

	replies := conn.replies(t, key)
	for i, msgID := range []int64{1 << 32, 1<<32 + 1} {
		e := &mt.RPCError{}
		if err := e.Decode(&bin.Buffer{Buf: findResult(t, replies, msgID)}); err != nil {
			t.Fatalf("entry %d: decode rpc error: %v", i+1, err)
		}
		if e.ErrorCode != 500 || e.ErrorMessage != "INTERNAL" {
			t.Fatalf("entry %d: rpc error = %d %q, want generic INTERNAL",
				i+1, e.ErrorCode, e.ErrorMessage)
		}
	}
}

// TestRPCDeadlineAbandonsWithoutTearingDownConnection proves all three halves
// of the per-request ceiling at once: a handler still running when its request
// deadline fires gets its context cancelled, the client receives the same
// generic INTERNAL any transient failure produces, and the connection itself
// survives — the next frame is served normally under a full, fresh budget, so
// a chunked transfer is bounded per chunk and never per logical transfer.
func TestRPCDeadlineAbandonsWithoutTearingDownConnection(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	ks := &statusKeyStore{key: key, users: []int64{0}}

	const deadline = 150 * time.Millisecond
	var mu sync.Mutex
	var budgets []time.Duration
	h := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		if dl, ok := req.Ctx.Deadline(); ok {
			mu.Lock()
			budgets = append(budgets, time.Until(dl))
			mu.Unlock()
		} else {
			t.Error("request context carries no deadline")
		}
		// Hold the RPC until the ceiling cancels it, the way a wedged store
		// call would hold one.
		<-req.Ctx.Done()
		return req.Ctx.Err()
	})
	srv := mtproto.New(exchange.PrivateKey{}, 2, ks, h, nil)
	if err := srv.SetRPCDeadline(deadline); err != nil {
		t.Fatalf("set rpc deadline: %v", err)
	}

	conn := &recordingFrameConn{
		frames: [][]byte{
			statusClientFrame(t, key, 42, 1<<32, &tg.AccountRegisterDeviceRequest{}),
			statusClientFrame(t, key, 42, 2<<32, &tg.AccountRegisterDeviceRequest{}),
		},
	}
	// The serve loop must run both frames to EOF: the deadline abandons the
	// request, never the connection.
	if err := srv.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want EOF (the connection survived)", err)
	}

	replies := conn.replies(t, key)
	decodeErr := func(msgID int64) *mt.RPCError {
		e := &mt.RPCError{}
		if err := e.Decode(&bin.Buffer{Buf: findResult(t, replies, msgID)}); err != nil {
			t.Fatalf("decode rpc error for msg id %d: %v", msgID, err)
		}
		return e
	}
	firstErr, secondErr := decodeErr(1<<32), decodeErr(2<<32)
	for i, e := range []*mt.RPCError{firstErr, secondErr} {
		if e.ErrorCode != 500 || e.ErrorMessage != "INTERNAL" {
			t.Fatalf("request %d: rpc error = %d %q, want generic INTERNAL",
				i+1, e.ErrorCode, e.ErrorMessage)
		}
	}

	// The second request started with a full budget of its own: per-chunk
	// bounding, not a shared pool that a long transfer drains.
	mu.Lock()
	defer mu.Unlock()
	if len(budgets) != 2 {
		t.Fatalf("handler saw %d requests, want 2", len(budgets))
	}
	if budgets[0] <= 0 || budgets[1] <= 0 {
		t.Fatalf("budgets = %v, want both positive", budgets)
	}
	if budgets[1] < deadline/2 {
		t.Fatalf("second request budget = %s, want a fresh full budget (>=%s)",
			budgets[1], deadline/2)
	}
}
