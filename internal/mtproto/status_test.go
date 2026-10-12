package mtproto_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

// statusEvent records a single onStatusChange callback invocation.
type statusEvent struct {
	userID int64
	online bool
}

// statusKeyStore is like bindingKeyStore but also records the status callback
// invocations so the test can assert the lifecycle sequence. provisional
// is the flag the store reports for every lookup, so a test that needs a
// provisional session sets it once and the serve loop delivers it to the
// handler on every call.
type statusKeyStore struct {
	key         crypto.AuthKey
	users       []int64
	provisional bool
	n           int
	events      []statusEvent
}

func (s *statusKeyStore) Save(context.Context, crypto.AuthKey) error { return nil }
func (s *statusKeyStore) Touch(context.Context, [8]byte) error       { return nil }

func (s *statusKeyStore) Get(context.Context, [8]byte, time.Duration) (crypto.AuthKey, int64, bool, mtproto.PendingLogin, bool, error) {
	userID := s.users[min(s.n, len(s.users)-1)]
	s.n++
	return s.key, userID, s.provisional, mtproto.PendingLogin{}, true, nil
}

func (s *statusKeyStore) OnStatusChange(_ context.Context, userID int64, online bool) {
	s.events = append(s.events, statusEvent{userID, online})
}

// statusFrameConn is a frameConn for status lifecycle tests.
type statusFrameConn struct {
	frames [][]byte
	i      int
	before func(served int)
}

func (c *statusFrameConn) Recv(_ context.Context, b *bin.Buffer) error {
	if c.before != nil {
		c.before(c.i)
	}
	if c.i >= len(c.frames) {
		return io.EOF
	}
	b.ResetTo(slices.Clone(c.frames[c.i]))
	c.i++
	return nil
}

func (c *statusFrameConn) Send(context.Context, *bin.Buffer) error { return nil }
func (c *statusFrameConn) Close() error                            { return nil }

// statusClientFrame encrypts body the way a real client puts it on the wire.
func statusClientFrame(t *testing.T, key crypto.AuthKey, sessionID, msgID int64, body bin.Encoder) []byte {
	t.Helper()
	var b bin.Buffer
	if err := body.Encode(&b); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	data := crypto.EncryptedMessageData{
		SessionID:              sessionID,
		MessageID:              msgID,
		MessageDataLen:         int32(b.Len()), //nolint:gosec // test frames contain one small PingRequest body
		MessageDataWithPadding: b.Copy(),
	}
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &b); err != nil {
		t.Fatalf("encrypt frame: %v", err)
	}
	return b.Copy()
}

// TestServeConnStatusCallbackFiresOnBind proves the onStatusChange callback
// fires with online=true when a session binds to a user, and offline=true
// when the last connection drops.
func TestServeConnStatusCallbackFiresOnBind(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	ks := &statusKeyStore{key: key, users: []int64{0, 7}}
	srv := mtproto.New(exchange.PrivateKey{}, 2, ks, nil, nil)
	srv.OnStatusChange(ks.OnStatusChange)

	frames := make([][]byte, len(ks.users))
	for i := range frames {
		frames[i] = statusClientFrame(t, key, 42, int64(i+1)<<32, &mt.PingRequest{PingID: int64(i + 1)})
	}

	err := srv.ServeConn(context.Background(), &statusFrameConn{frames: frames})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	// bind(7) → online=true, deferred bind(0) → offline=true.
	if len(ks.events) != 2 {
		t.Fatalf("events = %v, want [online=7, offline=7]", ks.events)
	}
	wantOnline := statusEvent{7, true}
	if ks.events[0] != wantOnline {
		t.Fatalf("event[0] = %+v, want %+v", ks.events[0], wantOnline)
	}
	wantOffline := statusEvent{7, false}
	if ks.events[1] != wantOffline {
		t.Fatalf("event[1] = %+v, want %+v", ks.events[1], wantOffline)
	}
}

// TestServeConnStatusCallbackNoFireOnRebind proves that rebinding from one
// user to another fires offline for the old user only when their last
// connection drops.
func TestServeConnStatusCallbackNoFireOnRebind(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	ks := &statusKeyStore{key: key, users: []int64{7, 9}}
	srv := mtproto.New(exchange.PrivateKey{}, 2, ks, nil, nil)
	srv.OnStatusChange(ks.OnStatusChange)

	// Pre-register a second conn for user 7 so that removing the tested conn
	// does not take user 7's count to zero.
	extraConn := mtproto.NewTestConn(&statusFrameConn{}, key)
	srv.Registry().Add(7, extraConn)

	frames := make([][]byte, len(ks.users))
	for i := range frames {
		frames[i] = statusClientFrame(t, key, 42, int64(i+1)<<32, &mt.PingRequest{PingID: int64(i + 1)})
	}

	err := srv.ServeConn(context.Background(), &statusFrameConn{frames: frames})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	// Frame 1: key resolves to 7 → bind(7) fires online=7.
	// Frame 2: key resolves to 9 → bind removes from 7 (extraConn remains, no offline),
	//   adds to 9 → online=9.
	// Deferred bind(0): removes from 9 (last conn) → offline=9.
	if len(ks.events) != 3 {
		t.Fatalf("events = %v, want [online=7, online=9, offline=9]", ks.events)
	}
	if ks.events[0] != (statusEvent{7, true}) {
		t.Fatalf("event[0] = %+v, want {7, true}", ks.events[0])
	}
	if ks.events[1] != (statusEvent{9, true}) {
		t.Fatalf("event[1] = %+v, want {9, true}", ks.events[1])
	}
	if ks.events[2] != (statusEvent{9, false}) {
		t.Fatalf("event[2] = %+v, want {9, false}", ks.events[2])
	}
}

// TestServeConnStatusCallbackNoOfflineWithExtraConn proves that dropping a
// connection does not fire offline when another connection for the same user
// remains in the registry.
func TestServeConnStatusCallbackNoOfflineWithExtraConn(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	ks := &statusKeyStore{key: key, users: []int64{7}}
	srv := mtproto.New(exchange.PrivateKey{}, 2, ks, nil, nil)
	srv.OnStatusChange(ks.OnStatusChange)

	frames := [][]byte{statusClientFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})}

	// Pre-populate registry with a second conn so the served conn is not the last.
	extraConn := mtproto.NewTestConn(&statusFrameConn{}, key)
	srv.Registry().Add(7, extraConn)

	err := srv.ServeConn(context.Background(), &statusFrameConn{frames: frames})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}

	// bind(7) → online, deferred bind(0) → NOT offline (extraConn still there).
	if len(ks.events) != 1 {
		t.Fatalf("events = %v, want [online=7]", ks.events)
	}
	if ks.events[0] != (statusEvent{7, true}) {
		t.Fatalf("event[0] = %+v, want {7, true}", ks.events[0])
	}
}

func TestServeConnStatusReconnectOvertakesOffline(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	ks := &statusKeyStore{key: key, users: []int64{7, 7}}
	srv := mtproto.New(exchange.PrivateKey{}, 2, ks, nil, nil)

	var storedOnline atomic.Bool
	var pushedOnline atomic.Bool
	var blockOffline sync.Once
	offlineEntered := make(chan struct{})
	releaseOffline := make(chan struct{})
	var releaseOfflineOnce sync.Once
	releaseOfflineNow := func() { releaseOfflineOnce.Do(func() { close(releaseOffline) }) }
	onlineEvents := make(chan struct{}, 2)
	srv.OnStatusChange(func(_ context.Context, userID int64, online bool) {
		if userID != 7 {
			t.Errorf("status callback user = %d, want 7", userID)
		}
		if !online {
			blockOffline.Do(func() {
				close(offlineEntered)
				<-releaseOffline
			})
		}
		storedOnline.Store(online)
		pushedOnline.Store(online)
		if online {
			onlineEvents <- struct{}{}
		}
	})

	firstDone := make(chan error, 1)
	firstFrame := statusClientFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})
	go func() {
		firstDone <- srv.ServeConn(context.Background(), &statusFrameConn{frames: [][]byte{firstFrame}})
	}()
	waitStatusSignal(t, onlineEvents, "first connection online")
	waitStatusSignal(t, offlineEntered, "first connection offline transition")

	holdSecond := make(chan struct{})
	var holdSecondOnce sync.Once
	releaseSecond := func() { holdSecondOnce.Do(func() { close(holdSecond) }) }
	t.Cleanup(func() {
		releaseOfflineNow()
		releaseSecond()
	})
	secondReadHeld := make(chan struct{})
	secondDone := make(chan error, 1)
	secondFrame := statusClientFrame(t, key, 43, 2<<32, &mt.PingRequest{PingID: 2})
	go func() {
		secondDone <- srv.ServeConn(context.Background(), &statusFrameConn{
			frames: [][]byte{secondFrame},
			before: func(served int) {
				if served == 1 {
					close(secondReadHeld)
					<-holdSecond
				}
			},
		})
	}()
	waitStatusSignal(t, onlineEvents, "reconnected connection online")
	waitStatusSignal(t, secondReadHeld, "reconnected connection remains live")

	releaseOfflineNow()
	if err := <-firstDone; !errors.Is(err, io.EOF) {
		t.Fatalf("first ServeConn = %v, want io.EOF", err)
	}
	if !storedOnline.Load() {
		t.Fatal("stored status ended offline after reconnect overtook the offline write")
	}
	if !pushedOnline.Load() {
		t.Fatal("last pushed status ended offline after reconnect overtook the offline write")
	}

	releaseSecond()
	if err := <-secondDone; !errors.Is(err, io.EOF) {
		t.Fatalf("second ServeConn = %v, want io.EOF", err)
	}
}

func TestStatusChangeCallbackRunsWithoutRegistryLock(t *testing.T) {
	t.Parallel()
	key := rebindTestKey()
	ks := &statusKeyStore{key: key, users: []int64{7}}
	srv := mtproto.New(exchange.PrivateKey{}, 2, ks, nil, nil)
	callbackStates := make(chan bool, 2)
	srv.OnStatusChange(func(_ context.Context, userID int64, online bool) {
		connected := len(srv.Registry().Conns(userID)) != 0
		callbackStates <- connected == online
	})

	frame := statusClientFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.ServeConn(context.Background(), &statusFrameConn{frames: [][]byte{frame}})
	}()

	for range 2 {
		select {
		case matches := <-callbackStates:
			if !matches {
				t.Fatal("registry state did not match callback status")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("status callback could not read the registry")
		}
	}
	if err := <-serveDone; !errors.Is(err, io.EOF) {
		t.Fatalf("ServeConn = %v, want io.EOF", err)
	}
}

func waitStatusSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
