package mtproto_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
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
	gotdmtproto "github.com/gotd/td/mtproto"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

// TestSlowRPCIsAcknowledgedBeforeClientResends drives a gotd client with its
// default retry settings against an RPC that takes longer than the client's
// resend interval. The server must acknowledge the request while it is still
// running, so the client sends it once and still receives the result.
func TestSlowRPCIsAcknowledgedBeforeClientResends(t *testing.T) {
	t.Parallel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	priv := exchange.PrivateKey{RSA: rsaKey}

	const handlerDelay = 7 * time.Second
	var (
		mu    sync.Mutex
		calls = map[int64]int{}
	)
	handler := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		var nearest tg.HelpGetNearestDCRequest
		if err := nearest.Decode(req.Buf); err != nil {
			return err
		}
		mu.Lock()
		calls[req.MsgID]++
		mu.Unlock()
		time.Sleep(handlerDelay)
		return c.SendResult(req, &tg.NearestDC{Country: "NL", ThisDC: 2, NearestDC: 2})
	})
	srv := mtproto.New(priv, 2, mtproto.NewMemoryAuthKeyStore(), handler, nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	served := make(chan error, 1)
	client := gotdmtproto.New(func(context.Context) (transport.Conn, error) {
		clientConn, serverConn := transport.Intermediate.Pipe()
		go func() { served <- srv.ServeConn(ctx, serverConn) }()
		return clientConn, nil
	}, gotdmtproto.Options{
		DC:         2,
		PublicKeys: []exchange.PublicKey{priv.Public()},
	})

	// The client runs until its context ends, so the callback stops it once the
	// call has been observed.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var invokeErr error
	err = client.Run(runCtx, func(ctx context.Context) error {
		defer stop()
		var result tg.NearestDC
		if invokeErr = client.Invoke(ctx, &tg.HelpGetNearestDCRequest{}, &result); invokeErr != nil {
			return invokeErr
		}
		if result.Country != "NL" {
			t.Errorf("nearest DC country = %q, want NL", result.Country)
		}
		// A resend parked behind the slow handler reaches it as soon as the
		// first call returns; give it the time to show up.
		time.Sleep(500 * time.Millisecond)
		return nil
	})
	if invokeErr != nil {
		t.Fatalf("invoke: %v", invokeErr)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("client run: %v", err)
	}
	<-served

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("handler saw %d distinct requests, want 1: %v", len(calls), calls)
	}
	for msgID, n := range calls {
		if n != 1 {
			t.Fatalf("request %d reached the handler %d times, want 1: the client resent it because the server never acknowledged it", msgID, n)
		}
	}
}

// ackEntry is one message of a scripted client frame.
type ackEntry struct {
	msgID int64
	seqNo int
	body  bin.Encoder
}

// seqClientFrame encrypts one client message carrying seqNo, which the
// server reads to tell content messages from service ones.
func seqClientFrame(t *testing.T, key crypto.AuthKey, sessionID int64, e ackEntry) []byte {
	t.Helper()
	var b bin.Buffer
	if err := e.body.Encode(&b); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	data := crypto.EncryptedMessageData{
		SessionID:              sessionID,
		MessageID:              e.msgID,
		SeqNo:                  int32(e.seqNo), //nolint:gosec // small test seqno
		MessageDataLen:         int32(b.Len()), //nolint:gosec // test frames hold a few small bodies
		MessageDataWithPadding: b.Copy(),
	}
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &b); err != nil {
		t.Fatalf("encrypt frame: %v", err)
	}
	return b.Copy()
}

// ackContainer wraps entries in a message container.
type ackContainer []ackEntry

func (c ackContainer) Encode(b *bin.Buffer) error {
	msgs := make([]proto.Message, len(c))
	for i, e := range c {
		var body bin.Buffer
		if err := e.body.Encode(&body); err != nil {
			return err
		}
		msgs[i] = proto.Message{ID: e.msgID, SeqNo: e.seqNo, Bytes: body.Len(), Body: body.Buf}
	}
	return (&proto.MessageContainer{Messages: msgs}).Encode(b)
}

// lingeringFrameConn is a recordingFrameConn that keeps the connection open
// for linger after its last frame, so a delayed server write can still land.
type lingeringFrameConn struct {
	recordingFrameConn

	linger time.Duration
}

func (c *lingeringFrameConn) Recv(ctx context.Context, b *bin.Buffer) error {
	if c.i >= len(c.frames) {
		time.Sleep(c.linger)
	}
	return c.recordingFrameConn.Recv(ctx, b)
}

// serverAcks returns the message ids named by every msgs_ack in bodies, one
// slice per acknowledgement, and the index of each in bodies.
func serverAcks(bodies [][]byte) (acks [][]int64, at []int) {
	for i, body := range bodies {
		var ack mt.MsgsAck
		if err := ack.Decode(&bin.Buffer{Buf: body}); err != nil {
			continue
		}
		acks = append(acks, ack.MsgIDs)
		at = append(at, i)
	}
	return acks, at
}

// TestContainerContentMessagesAreAcknowledgedUpfront sends a container whose
// first RPC outlasts the acknowledgement delay. Every content message in it is
// acknowledged while that RPC still runs, including the one queued behind it,
// and the acknowledgements, containers and even-seqno messages are not.
func TestContainerContentMessagesAreAcknowledgedUpfront(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()

	const slowID, queuedID, ackID, pingID, containerID = 1 << 32, 2 << 32, 3 << 32, 4 << 32, 5 << 32
	handler := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		if req.MsgID == slowID {
			time.Sleep(1500 * time.Millisecond)
		}
		return c.SendResult(req, &tg.NearestDC{Country: "NL", ThisDC: 2, NearestDC: 2})
	})
	srv := mtproto.New(exchange.PrivateKey{}, 2, &statusKeyStore{key: key, users: []int64{0}}, handler, nil)
	conn := &recordingFrameConn{frames: [][]byte{seqClientFrame(t, key, 42, ackEntry{
		msgID: containerID,
		seqNo: 7,
		body: ackContainer{
			{msgID: slowID, seqNo: 1, body: &tg.HelpGetNearestDCRequest{}},
			{msgID: queuedID, seqNo: 3, body: &tg.HelpGetNearestDCRequest{}},
			{msgID: ackID, seqNo: 5, body: &mt.MsgsAck{MsgIDs: []int64{9 << 32}}},
			{msgID: pingID, seqNo: 6, body: &mt.PingRequest{PingID: 1}},
		},
	})}}
	if err := srv.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	bodies := conn.replies(t, key)
	acks, at := serverAcks(bodies)
	if len(acks) != 1 {
		t.Fatalf("server sent %d msgs_ack, want 1: %v", len(acks), acks)
	}
	if !slices.Equal(acks[0], []int64{slowID, queuedID}) {
		t.Fatalf("msgs_ack names %v, want the two RPCs %v", acks[0], []int64{slowID, queuedID})
	}
	findResult(t, bodies, slowID)
	findResult(t, bodies, queuedID)
	for i, body := range bodies {
		var res proto.Result
		if err := res.Decode(&bin.Buffer{Buf: body}); err == nil && res.RequestMessageID == slowID && i < at[0] {
			t.Fatalf("result for the slow RPC at reply %d precedes its acknowledgement at %d", i, at[0])
		}
	}
}

// TestPromptReplyNeedsNoSeparateAck answers a content message within the
// acknowledgement delay and keeps the connection open past it: the reply is
// the acknowledgement, so no msgs_ack follows.
func TestPromptReplyNeedsNoSeparateAck(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()

	handler := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		return c.SendResult(req, &tg.NearestDC{Country: "NL", ThisDC: 2, NearestDC: 2})
	})
	srv := mtproto.New(exchange.PrivateKey{}, 2, &statusKeyStore{key: key, users: []int64{0}}, handler, nil)
	conn := &lingeringFrameConn{
		frames: [][]byte{
			seqClientFrame(t, key, 42, ackEntry{msgID: 1 << 32, seqNo: 1, body: &tg.HelpGetNearestDCRequest{}}),
			seqClientFrame(t, key, 42, ackEntry{msgID: 2 << 32, seqNo: 3, body: &mt.PingRequest{PingID: 1}}),
		},
		linger: 1500 * time.Millisecond,
	}
	if err := srv.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	bodies := conn.replies(t, key)
	if acks, _ := serverAcks(bodies); len(acks) != 0 {
		t.Fatalf("server sent msgs_ack %v for messages it had already answered", acks)
	}
	findResult(t, bodies, 1<<32)
}
