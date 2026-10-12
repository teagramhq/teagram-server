package mtproto

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/clock"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"
)

func TestStaleSocketCannotRefreshAuthorizationAfterRebind(t *testing.T) {
	t.Parallel()

	key := authorizationRebindTestKey()
	store := &authorizationRebindKeyStore{
		key:   key,
		users: []int64{7, 9},
		owner: 7,
		metadata: AuthorizationMetadata{
			DeviceModel: "User A device", Platform: "a-platform", SystemVersion: "A OS",
			APIID: 1, AppVersion: "1.0",
		},
		ip: "192.0.2.10",
	}
	handler := UnpackInvoke(HandlerFunc(func(c *Conn, req *Request) error {
		return c.SendResult(req, &tg.BoolTrue{})
	}))
	server := New(exchange.PrivateKey{}, 2, store, handler, nil)
	server.clock = &advancingAuthorizationClock{nanos: atomic.Int64{}}

	frames := [][]byte{
		authorizationFrame(t, key, 42, 1<<32, &tg.InitConnectionRequest{
			APIID: 1, DeviceModel: "User A device", SystemVersion: "A OS", AppVersion: "1.0",
			LangPack: "a-platform", Query: &tg.HelpGetConfigRequest{},
		}),
		authorizationFrame(t, key, 42, 2<<32, &mt.PingRequest{PingID: 2}),
	}
	conn := &authorizationFrameConn{
		frames: frames,
		before: func(served int) {
			if served == 1 {
				// Another connection logged A out and rebound the key to B while
				// this socket stayed open. The next lookup sees only B.
				store.owner = 9
				store.metadata = AuthorizationMetadata{
					DeviceModel: "User B device", Platform: "b-platform", SystemVersion: "B OS",
					APIID: 2, AppVersion: "2.0",
				}
				store.ip = "198.51.100.20"
			}
		},
	}
	err := server.serveConn(context.Background(), conn, netip.MustParseAddr("203.0.113.7"), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("serveConn error = %v, want scripted EOF", err)
	}
	if got, want := store.metadata, (AuthorizationMetadata{
		DeviceModel: "User B device", Platform: "b-platform", SystemVersion: "B OS",
		APIID: 2, AppVersion: "2.0",
	}); got != want {
		t.Fatalf("authorization metadata after stale socket frame = %+v, want B's metadata %+v", got, want)
	}
	if got, want := store.ip, "198.51.100.20"; got != want {
		t.Fatalf("authorization IP after stale socket frame = %q, want %q", got, want)
	}
	if store.metadataTouches != 1 {
		t.Fatalf("metadata touches = %d, want only A's initial touch", store.metadataTouches)
	}
	if store.lastSeenTouches != 1 {
		t.Fatalf("plain last-seen touches = %d, want one touch for the stale socket", store.lastSeenTouches)
	}
}

type authorizationRebindKeyStore struct {
	key             crypto.AuthKey
	users           []int64
	lookups         int
	owner           int64
	metadata        AuthorizationMetadata
	ip              string
	metadataTouches int
	lastSeenTouches int
}

func (s *authorizationRebindKeyStore) Save(context.Context, crypto.AuthKey) error { return nil }

func (s *authorizationRebindKeyStore) Get(context.Context, [8]byte, time.Duration) (crypto.AuthKey, int64, bool, PendingLogin, bool, error) {
	userID := s.users[min(s.lookups, len(s.users)-1)]
	s.lookups++
	return s.key, userID, false, PendingLogin{}, true, nil
}

func (s *authorizationRebindKeyStore) Touch(context.Context, [8]byte) error {
	s.lastSeenTouches++
	return nil
}

func (s *authorizationRebindKeyStore) TouchWithMetadata(_ context.Context, _ [8]byte, addr netip.Addr, expectedUserID int64, metadata *AuthorizationMetadata) error {
	s.metadataTouches++
	if s.owner != expectedUserID {
		return nil
	}
	if addr.IsValid() {
		s.ip = addr.Unmap().String()
	}
	if metadata != nil {
		s.metadata = *metadata
	}
	return nil
}

type authorizationFrameConn struct {
	frames [][]byte
	i      int
	before func(served int)
}

func (c *authorizationFrameConn) Recv(_ context.Context, b *bin.Buffer) error {
	if c.before != nil {
		c.before(c.i)
	}
	if c.i >= len(c.frames) {
		return io.EOF
	}
	b.ResetTo(c.frames[c.i])
	c.i++
	return nil
}

func (*authorizationFrameConn) Send(context.Context, *bin.Buffer) error { return nil }
func (*authorizationFrameConn) Close() error                            { return nil }

type advancingAuthorizationClock struct {
	nanos atomic.Int64
}

func (c *advancingAuthorizationClock) Now() time.Time {
	return time.Unix(0, c.nanos.Add(int64(2*time.Minute)))
}

func (*advancingAuthorizationClock) Timer(d time.Duration) clock.Timer {
	return clock.System.Timer(d)
}

func (*advancingAuthorizationClock) Ticker(d time.Duration) clock.Ticker {
	return clock.System.Ticker(d)
}

func authorizationFrame(t *testing.T, key crypto.AuthKey, sessionID, msgID int64, body bin.Encoder) []byte {
	t.Helper()
	var b bin.Buffer
	if err := body.Encode(&b); err != nil {
		t.Fatalf("encode authorization test frame: %v", err)
	}
	data := crypto.EncryptedMessageData{
		SessionID:              sessionID,
		MessageID:              msgID,
		MessageDataLen:         int32(b.Len()), //nolint:gosec // test messages are far below MaxInt32
		MessageDataWithPadding: b.Copy(),
	}
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &b); err != nil {
		t.Fatalf("encrypt authorization test frame: %v", err)
	}
	return b.Copy()
}

func authorizationRebindTestKey() crypto.AuthKey {
	var raw crypto.Key
	for i := range raw {
		raw[i] = byte(i)
	}
	return raw.WithID()
}

var _ AuthKeyMetadataToucher = (*authorizationRebindKeyStore)(nil)
var _ transport.Conn = (*authorizationFrameConn)(nil)
var _ clock.Clock = (*advancingAuthorizationClock)(nil)
