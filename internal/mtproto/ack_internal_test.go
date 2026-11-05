package mtproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/clock"
	tdcrypto "github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"
)

func ackTestKey() tdcrypto.AuthKey {
	var raw tdcrypto.Key
	for i := range raw {
		raw[i] = byte(i + 7)
	}
	return raw.WithID()
}

// ackTestFrame encrypts one content message (odd seqno) from the client.
func ackTestFrame(t *testing.T, key tdcrypto.AuthKey, msgID int64, body bin.Encoder) []byte {
	t.Helper()
	var b bin.Buffer
	if err := body.Encode(&b); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	data := tdcrypto.EncryptedMessageData{
		SessionID:              42,
		MessageID:              msgID,
		SeqNo:                  1,
		MessageDataLen:         int32(b.Len()), //nolint:gosec // small test body
		MessageDataWithPadding: b.Copy(),
	}
	if err := tdcrypto.NewClientCipher(tdcrypto.DefaultRand()).Encrypt(key, data, &b); err != nil {
		t.Fatalf("encrypt frame: %v", err)
	}
	return b.Copy()
}

// A queued read-ahead goroutine must not restore old acknowledgements after a
// binding change cleared the queue. Hold writeMu until that change is complete
// so enqueue cannot win the race, for both standalone and container messages.
func TestReadAheadAcksDropStaleBinding(t *testing.T) {
	for _, container := range []bool{false, true} {
		for _, change := range []string{"session", "key", "unchanged"} {
			t.Run(fmt.Sprintf("container=%t/%s", container, change), func(t *testing.T) {
				t.Parallel()
				key := ackTestKey()
				client, server := transport.Intermediate.Pipe()
				c := newConn(server, tdcrypto.NewServerCipher(tdcrypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, time.Second, slog.Default())
				c.setKey(key)
				c.setSession(42)
				t.Cleanup(func() {
					c.stopAcks()
					c.writeMu.Lock()
					if c.ackTimer != nil {
						c.ackTimer.Stop()
					}
					c.writeMu.Unlock()
					if err := client.Close(); err != nil {
						t.Error(err)
					}
					if err := server.Close(); err != nil {
						t.Error(err)
					}
				})
				const staleID, freshID = 1 << 32, 2 << 32
				var body bin.Encoder = &tg.HelpGetNearestDCRequest{}
				if container {
					var inner bin.Buffer
					if err := body.Encode(&inner); err != nil {
						t.Fatal(err)
					}
					body = &proto.MessageContainer{Messages: []proto.Message{{ID: staleID, SeqNo: 1, Bytes: inner.Len(), Body: inner.Copy()}}}
				}
				frame := ackTestFrame(t, key, staleID, body)
				c.writeMu.Lock()
				c.queueReadAheadAcks(frame)
				// Reproduce the binding update and queue reset performed under
				// writeMu by setSession/setKey, before deferred enqueue can run.
				switch change {
				case "session":
					c.sessionID = 43
				case "key":
					raw := key.Value
					raw[0] ^= 1
					key = raw.WithID()
					c.authKey = key
				}
				c.pendingAcks = nil
				c.writeMu.Unlock()
				c.queueAcks(freshID)

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var wire bin.Buffer
				if err := client.Recv(ctx, &wire); err != nil {
					t.Fatalf("receive acknowledgement: %v", err)
				}
				msg, err := tdcrypto.NewClientCipher(tdcrypto.DefaultRand()).DecryptFromBuffer(key, &wire)
				if err != nil {
					t.Fatal(err)
				}
				var ack mt.MsgsAck
				if err := ack.Decode(&bin.Buffer{Buf: msg.Data()}); err != nil {
					t.Fatal(err)
				}
				slices.Sort(ack.MsgIDs)
				want := []int64{freshID}
				if change == "unchanged" {
					want = []int64{staleID, freshID}
				}
				if !slices.Equal(ack.MsgIDs, want) {
					t.Fatalf("acknowledged %v, want %v after %s binding", ack.MsgIDs, want, change)
				}
			})
		}
	}
}

// TestReadAheadFrameIsAcknowledgedDuringSlowRPC sends four requests in
// separate frames while the first one's handler is still running. The serve
// loop dispatches the queued requests only after the first finishes, so they must be
// acknowledged from the frame read ahead, or a client resends it.
func TestReadAheadFrameIsAcknowledgedDuringSlowRPC(t *testing.T) {
	t.Parallel()

	key := ackTestKey()
	store := NewMemoryAuthKeyStore()
	if err := store.Save(context.Background(), key); err != nil {
		t.Fatalf("save key: %v", err)
	}
	const slowID, nextID, thirdID, fourthID = 1 << 32, 2 << 32, 3 << 32, 4 << 32
	handler := HandlerFunc(func(c *Conn, req *Request) error {
		if req.MsgID == slowID {
			time.Sleep(2500 * time.Millisecond)
		}
		return c.SendResult(req, &tg.NearestDC{Country: "NL", ThisDC: 2, NearestDC: 2})
	})
	srv := New(exchange.PrivateKey{}, 2, store, handler, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, server := transport.Intermediate.Pipe()
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	served := make(chan error, 1)
	go func() {
		served <- srv.serveConnWithContexts(ctx, ctx, server, netip.Addr{}, nil, nil)
	}()

	// The pipe is synchronous, so the client reads on its own goroutine while
	// it sends.
	cipher := tdcrypto.NewClientCipher(tdcrypto.DefaultRand())
	bodies := make(chan []byte, 16)
	go func() {
		defer close(bodies)
		for {
			var b bin.Buffer
			if err := client.Recv(ctx, &b); err != nil {
				return
			}
			msg, err := cipher.DecryptFromBuffer(key, &b)
			if err != nil {
				return
			}
			bodies <- slices.Clone(msg.Data())
		}
	}()
	for _, f := range [][]byte{
		ackTestFrame(t, key, slowID, &tg.HelpGetNearestDCRequest{}),
		ackTestFrame(t, key, nextID, &tg.HelpGetNearestDCRequest{}),
		ackTestFrame(t, key, thirdID, &tg.HelpGetNearestDCRequest{}),
		ackTestFrame(t, key, fourthID, &tg.HelpGetNearestDCRequest{}),
	} {
		if err := client.Send(ctx, &bin.Buffer{Buf: f}); err != nil {
			t.Fatalf("send frame: %v", err)
		}
	}

	var acked []int64
	for answered := false; !answered; {
		body, ok := <-bodies
		if !ok {
			t.Fatal("connection ended before the slow request was answered")
		}
		var ack mt.MsgsAck
		if err := ack.Decode(&bin.Buffer{Buf: body}); err == nil {
			acked = append(acked, ack.MsgIDs...)
			continue
		}
		var res proto.Result
		if err := res.Decode(&bin.Buffer{Buf: body}); err == nil {
			if res.RequestMessageID != slowID {
				t.Fatalf("first result answers %d, want the slow request %d", res.RequestMessageID, slowID)
			}
			answered = true
		}
	}
	for _, id := range []int64{slowID, nextID, thirdID, fourthID} {
		if !slices.Contains(acked, id) {
			t.Fatalf("acknowledged %v before the slow request finished, want %d among them", acked, id)
		}
	}

	for _, want := range []int64{nextID, thirdID, fourthID} {
		for {
			body, ok := <-bodies
			if !ok {
				t.Fatal("connection ended before all queued replies")
			}
			var result proto.Result
			if err := result.Decode(&bin.Buffer{Buf: body}); err != nil {
				continue
			}
			if result.RequestMessageID != want {
				t.Fatalf("reply order: got %d, want %d", result.RequestMessageID, want)
			}
			break
		}
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	for range bodies {
	}
	<-served
}

// lockedLogSink records slog records from any goroutine.
type lockedLogSink struct {
	mu      sync.Mutex
	records []serverErrorLogRecord
}

func (*lockedLogSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *lockedLogSink) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, serverErrorLogRecord{message: record.Message, level: record.Level, attrs: attrs})
	return nil
}

func (s *lockedLogSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *lockedLogSink) WithGroup(string) slog.Handler      { return s }

// failAfterConn delivers its frames, accepts the first ok sends and fails every
// later one, while Close always succeeds.
type failAfterConn struct {
	frames  [][]byte
	ok      int
	sendErr error

	mu    sync.Mutex
	sends int
}

func (c *failAfterConn) Recv(_ context.Context, b *bin.Buffer) error {
	if len(c.frames) == 0 {
		return io.EOF
	}
	b.ResetTo(slices.Clone(c.frames[0]))
	c.frames = c.frames[1:]
	return nil
}

func (c *failAfterConn) Send(context.Context, *bin.Buffer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends++
	if c.sends > c.ok {
		return c.sendErr
	}
	return nil
}

func (*failAfterConn) Close() error { return nil }

type failAckEncryptionReader struct {
	reads int
	err   error
}

func (r *failAckEncryptionReader) Read(p []byte) (int, error) {
	if r.reads >= 2 {
		return 0, r.err
	}
	r.reads++
	clear(p)
	return len(p), nil
}

// TestFailedAckLogPreservesSendClassification verifies that a failed ack
// retains its RPC type without turning an encryption failure into a transport
// failure. Only an error returned by the socket write is transport-classified.
func TestFailedAckLogPreservesSendClassification(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		writeErr          error
		encryptErr        error
		wantCause         requestFailureCause
		wantError         string
		wantErrorCategory RPCResultClass
		wantSends         int
	}{
		{
			name:              "write deadline",
			writeErr:          os.ErrDeadlineExceeded,
			wantCause:         requestFailureTransport,
			wantError:         "transport",
			wantErrorCategory: RPCResultTransportFailure,
			wantSends:         3,
		},
		{
			name:              "encryption failure",
			encryptErr:        errors.New("PRIVATE-ACK-ENCRYPT-DETAIL"),
			wantCause:         requestFailureOther,
			wantError:         "internal",
			wantErrorCategory: RPCResultInternal,
			wantSends:         1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			const rawWriteError = "PRIVATE-ACK-WRITE-DETAIL"
			const rawEncryptError = "PRIVATE-ACK-ENCRYPT-DETAIL"

			key := ackTestKey()
			store := NewMemoryAuthKeyStore()
			if err := store.Save(context.Background(), key); err != nil {
				t.Fatalf("save key: %v", err)
			}
			handler := HandlerFunc(func(c *Conn, req *Request) error {
				time.Sleep(ackDelay + 250*time.Millisecond)
				return c.SendResult(req, &tg.NearestDC{Country: "NL", ThisDC: 2, NearestDC: 2})
			})
			sink := &lockedLogSink{}
			srv := New(exchange.PrivateKey{}, 2, store, handler, slog.New(sink))
			if test.encryptErr != nil {
				srv.cipher = tdcrypto.NewServerCipher(&failAckEncryptionReader{err: test.encryptErr})
			}
			conn := &failAfterConn{
				frames:  [][]byte{ackTestFrame(t, key, 1<<32, &tg.HelpGetNearestDCRequest{})},
				ok:      1,
				sendErr: test.writeErr,
			}
			if err := srv.ServeConn(context.Background(), conn); err == nil {
				t.Fatal("ServeConn returned nil after its writes failed")
			}
			conn.mu.Lock()
			sends := conn.sends
			conn.mu.Unlock()
			if sends != test.wantSends {
				t.Fatalf("transport sends = %d, want %d", sends, test.wantSends)
			}

			sink.mu.Lock()
			defer sink.mu.Unlock()
			for _, r := range sink.records {
				if r.message != "server operation failed" || r.attrs["operation"] != "msgs_ack" {
					continue
				}
				if r.attrs["category"] != string(serverFailureRequest) {
					t.Fatalf("msgs_ack failure category = %v, want %s", r.attrs["category"], serverFailureRequest)
				}
				for key, want := range map[string]any{
					"rpc_method":     UnknownRPCMethod,
					"rpc_type_id":    uint64(tg.HelpGetNearestDCRequestTypeID),
					"request_id":     int64(0),
					"error_category": string(test.wantErrorCategory),
					"cause":          string(test.wantCause),
					"error":          test.wantError,
				} {
					if got := r.attrs[key]; got != want {
						t.Errorf("msgs_ack %s = %v, want %v", key, got, want)
					}
				}
				if strings.Contains(r.message+fmt.Sprint(r.attrs), rawWriteError) ||
					strings.Contains(r.message+fmt.Sprint(r.attrs), rawEncryptError) {
					t.Errorf("msgs_ack failure log exposed raw error: %+v", r)
				}
				return
			}
			t.Fatalf("no server failure logged for the failed msgs_ack write: %+v", sink.records)
		})
	}
}

func TestLateReadAheadAckKeepsDispatchedLeafType(t *testing.T) {
	t.Parallel()

	key := ackTestKey()
	const messageID = 1 << 32
	c := &Conn{authKey: key, sessionID: 42}
	t.Cleanup(func() {
		c.stopAcks()
		c.writeMu.Lock()
		if c.ackTimer != nil {
			c.ackTimer.Stop()
		}
		c.writeMu.Unlock()
	})

	// handleWithHandler records the top-level ID, then dispatch unwraps to the
	// leaf. A delayed read-ahead enqueue must not restore that outer wrapper.
	c.queueInitialRPCAck(messageID, tg.InvokeWithLayerRequestTypeID)
	c.queueRPCAck(messageID, tg.AuthSignUpRequestTypeID)
	c.queueBoundAckRecords(key.ID, 42, queuedAck{
		messageID:   messageID,
		rpcTypeID:   tg.InvokeWithLayerRequestTypeID,
		typeIDKnown: true,
	})

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(c.pendingAcks) != 1 {
		t.Fatalf("pending acks = %+v, want one ack", c.pendingAcks)
	}
	if got, want := c.pendingAcks[0].rpcTypeID, uint32(tg.AuthSignUpRequestTypeID); got != want {
		t.Fatalf("RPC type ID = %#x, want dispatched leaf %#x", got, want)
	}
}
