package mtproto

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
)

type timedReadFrame struct {
	data  []byte
	delay time.Duration
}

type idleReadTestConn struct {
	mu           sync.Mutex
	frames       []timedReadFrame
	index        int
	blockWhenOut bool
	blockedAt    chan time.Time
	deadlines    []time.Time
	readStarts   []time.Time
	timeoutErr   error
}

func (c *idleReadTestConn) Recv(ctx context.Context, b *bin.Buffer) error {
	c.mu.Lock()
	c.readStarts = append(c.readStarts, time.Now())
	deadline, _ := ctx.Deadline()
	c.deadlines = append(c.deadlines, deadline)
	if c.index < len(c.frames) {
		frame := c.frames[c.index]
		c.index++
		c.mu.Unlock()
		if frame.delay > 0 {
			timer := time.NewTimer(frame.delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		b.ResetTo(slices.Clone(frame.data))
		return nil
	}
	blockWhenOut := c.blockWhenOut
	blockedAt := c.blockedAt
	c.mu.Unlock()
	if !blockWhenOut {
		return io.EOF
	}
	if blockedAt != nil {
		select {
		case blockedAt <- time.Now():
		default:
		}
	}
	<-ctx.Done()
	if c.timeoutErr != nil {
		return c.timeoutErr
	}
	return ctx.Err()
}

func (*idleReadTestConn) Send(context.Context, *bin.Buffer) error { return nil }
func (*idleReadTestConn) Close() error                            { return nil }

func (c *idleReadTestConn) readDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.deadlines)
}

func (c *idleReadTestConn) readStartTimes() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.readStarts)
}

func TestAuthenticatedConnectionUsesBoundedIdleReadTimeout(t *testing.T) {
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	conn := &idleReadTestConn{frames: []timedReadFrame{{data: idleReadPingFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})}}}
	server := New(exchange.PrivateKey{}, 2, keys, nil, nil)
	if err := server.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}
	deadlines := conn.readDeadlines()
	if len(deadlines) != 2 {
		t.Fatalf("recorded %d read deadlines, want 2", len(deadlines))
	}
	if remaining := time.Until(deadlines[1]); remaining < 70*time.Second || remaining > 75*time.Second {
		t.Fatalf("authenticated idle read deadline has %s remaining, want the bounded 75-second default", remaining)
	}
}

func TestOrdinaryPingsRenewAuthenticatedIdleTimeout(t *testing.T) {
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	conn := &idleReadTestConn{frames: []timedReadFrame{
		{data: idleReadPingFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})},
		{data: idleReadPingFrame(t, key, 42, 2<<32, &mt.PingRequest{PingID: 2}), delay: 50 * time.Millisecond},
		{data: idleReadPingFrame(t, key, 42, 3<<32, &mt.PingRequest{PingID: 3}), delay: 50 * time.Millisecond},
	}}
	server := New(exchange.PrivateKey{}, 2, keys, nil, nil)
	server.authenticatedReadTimeout = 80 * time.Millisecond
	if err := server.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want both ordinary pings to arrive before renewed inactivity deadlines", err)
	}
	deadlines := conn.readDeadlines()
	if len(deadlines) != 4 {
		t.Fatalf("recorded %d read deadlines, want 4", len(deadlines))
	}
	if !deadlines[2].After(deadlines[1]) {
		t.Fatalf("read deadline after second ordinary ping = %s, want later than first authenticated deadline %s", deadlines[2], deadlines[1])
	}
	if !deadlines[3].After(deadlines[2]) {
		t.Fatalf("read deadline after third ordinary ping = %s, want later than second authenticated deadline %s", deadlines[3], deadlines[2])
	}
}

func TestPingDelayDisconnectKeepsConnectionWithinRequestedDelay(t *testing.T) {
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	conn := &idleReadTestConn{frames: []timedReadFrame{
		{data: idleReadPingFrame(t, key, 42, 1<<32, &mt.PingDelayDisconnectRequest{PingID: 1, DisconnectDelay: 1})},
		{data: idleReadPingFrame(t, key, 42, 2<<32, &mt.PingDelayDisconnectRequest{PingID: 2, DisconnectDelay: 1}), delay: 200 * time.Millisecond},
	}}
	server := New(exchange.PrivateKey{}, 2, keys, nil, nil)
	server.readTimeout = 50 * time.Millisecond
	if err := server.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want the second ping to arrive within its disconnect delay", err)
	}
	deadlines := conn.readDeadlines()
	readStarts := conn.readStartTimes()
	if len(deadlines) != 3 {
		t.Fatalf("recorded %d read deadlines, want 3", len(deadlines))
	}
	for i, deadline := range deadlines[1:] {
		if delay := deadline.Sub(readStarts[i+1]); delay < 900*time.Millisecond || delay > time.Second {
			t.Fatalf("read %d deadline is %s after its start, want the requested 1 second", i+2, delay)
		}
	}
}

func TestAuthenticatedIdleTimeoutDoesNotResetWhenAuthKeyChanges(t *testing.T) {
	firstKey := idleReadTestKey()
	var otherRaw crypto.Key
	for i := range otherRaw {
		otherRaw[i] = byte(i + 21)
	}
	secondKey := otherRaw.WithID()
	keys := NewMemoryAuthKeyStore()
	for _, key := range []crypto.AuthKey{firstKey, secondKey} {
		if err := keys.Save(context.Background(), key); err != nil {
			t.Fatalf("save auth key: %v", err)
		}
	}
	conn := &idleReadTestConn{frames: []timedReadFrame{
		{data: idleReadPingFrame(t, firstKey, 42, 1<<32, &mt.PingDelayDisconnectRequest{PingID: 1, DisconnectDelay: 1})},
		{data: idleReadPingFrame(t, secondKey, 42, 2<<32, &mt.PingRequest{PingID: 2}), delay: 200 * time.Millisecond},
	}}
	server := New(exchange.PrivateKey{}, 2, keys, nil, nil)
	server.readTimeout = 50 * time.Millisecond
	if err := server.ServeConn(context.Background(), conn); !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}
	deadlines := conn.readDeadlines()
	if len(deadlines) != 3 {
		t.Fatalf("recorded %d read deadlines, want 3", len(deadlines))
	}
	if !deadlines[2].Equal(deadlines[1]) {
		t.Fatalf("read deadline after auth-key change = %s, want unchanged deadline %s", deadlines[2], deadlines[1])
	}
}

func TestMsgsAckTrafficDoesNotRenewPingDelayDisconnectDeadline(t *testing.T) {
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	const ackCount = 10
	frames := make([]timedReadFrame, 1, ackCount+1)
	frames[0] = timedReadFrame{data: idleReadPingFrame(t, key, 42, 1<<32, &mt.PingDelayDisconnectRequest{PingID: 1, DisconnectDelay: 1})}
	for i := range ackCount {
		frames = append(frames, timedReadFrame{
			data:  idleReadPingFrame(t, key, 42, int64(i+2)<<32, &mt.MsgsAck{MsgIDs: []int64{int64(i+1) << 32}}),
			delay: 200 * time.Millisecond,
		})
	}
	conn := &idleReadTestConn{frames: frames, blockWhenOut: true}
	server := New(exchange.PrivateKey{}, 2, keys, nil, nil)
	started := time.Now()
	err := server.ServeConn(context.Background(), conn)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ServeConn = %v, want the original ping delay deadline", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("ACK traffic kept the connection open for %s, want about the 1-second ping delay", elapsed)
	}
	deadlines := conn.readDeadlines()
	if len(deadlines) < 3 {
		t.Fatalf("recorded %d read deadlines, want ACK traffic followed by a timed-out read", len(deadlines))
	}
	for i, deadline := range deadlines[2:] {
		if !deadline.Equal(deadlines[1]) {
			t.Errorf("read %d deadline = %s, want unchanged ping deadline %s", i+3, deadline, deadlines[1])
		}
	}
}

func TestPingDelayDisconnectExpiresAfterRequestedDelay(t *testing.T) {
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	blockedAt := make(chan time.Time, 1)
	conn := &idleReadTestConn{
		frames:       []timedReadFrame{{data: idleReadPingFrame(t, key, 42, 1<<32, &mt.PingDelayDisconnectRequest{PingID: 1, DisconnectDelay: 1})}},
		blockWhenOut: true,
		blockedAt:    blockedAt,
	}
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, keys, nil, slog.New(sink))
	server.readTimeout = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(context.Background(), conn) }()
	started := <-blockedAt
	err := <-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ServeConn = %v, want a read timeout", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("idle connection closed after %s, want about the requested 1 second", elapsed)
	}
	server.logConnectionFailure(err)
	if len(sink.records) != 1 || sink.records[0].attrs["category"] != "idle_timeout" {
		t.Fatalf("idle read log = %+v, want category idle_timeout", sink.records)
	}
}

func TestIdleReadTimeoutClassifiesTransportDeadlineError(t *testing.T) {
	conn := &idleReadTestConn{
		blockWhenOut: true,
		timeoutErr:   errors.New("transport read deadline exceeded"),
	}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, nil)
	server.readTimeout = time.Millisecond
	err := server.read(context.Background(), conn, &bin.Buffer{}, time.Time{})
	if !errors.Is(err, errIdleReadTimeout) {
		t.Fatalf("read error = %v, want idle-timeout classification", err)
	}
	if isDisconnect(err) {
		t.Fatalf("idle read error %v was treated as an unclassified disconnect", err)
	}
}

func TestPingDelayDisconnectReadTimeoutIsBounded(t *testing.T) {
	const fallback = 75 * time.Second
	if got := pingDelayDisconnectReadTimeout(120, fallback); got != 120*time.Second {
		t.Fatalf("requested read timeout = %s, want 2 minutes", got)
	}
	if got := pingDelayDisconnectReadTimeout(int(maxPingDelayDisconnectReadTimeout/time.Second)+1, fallback); got != maxPingDelayDisconnectReadTimeout {
		t.Fatalf("oversized requested read timeout = %s, want cap %s", got, maxPingDelayDisconnectReadTimeout)
	}
	for _, delay := range []int{0, -1} {
		if got := pingDelayDisconnectReadTimeout(delay, fallback); got != fallback {
			t.Errorf("disconnect delay %d produced %s, want fallback %s", delay, got, fallback)
		}
	}
}

func idleReadTestKey() crypto.AuthKey {
	var raw crypto.Key
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	return raw.WithID()
}

func idleReadPingFrame(t *testing.T, key crypto.AuthKey, sessionID, msgID int64, request bin.Encoder) []byte {
	t.Helper()
	var body bin.Buffer
	if err := request.Encode(&body); err != nil {
		t.Fatalf("encode ping request: %v", err)
	}
	data := crypto.EncryptedMessageData{
		SessionID:              sessionID,
		MessageID:              msgID,
		MessageDataLen:         int32(body.Len()), //nolint:gosec // test ping frame is small
		MessageDataWithPadding: body.Copy(),
	}
	var frame bin.Buffer
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &frame); err != nil {
		t.Fatalf("encrypt ping frame: %v", err)
	}
	return frame.Copy()
}
