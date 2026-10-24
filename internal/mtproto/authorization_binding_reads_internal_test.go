package mtproto

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
)

func TestUnboundFramesReadAuthKeyOnceAcrossWrappersAndContainers(t *testing.T) {
	t.Parallel()

	key := authorizationRebindTestKey()
	store := &authorizationBindingReadKeyStore{key: key}
	handler := UnpackInvoke(HandlerFunc(func(c *Conn, req *Request) error {
		return c.SendResult(req, &tg.BoolTrue{})
	}))
	server := New(exchange.PrivateKey{}, 2, store, handler, nil)
	if err := server.SetMaxConnsPerUnboundKey(1); err != nil {
		t.Fatalf("set unbound-key cap: %v", err)
	}

	wrapped := &tg.InitConnectionRequest{
		APIID: 1,
		Query: &tg.HelpGetConfigRequest{},
	}
	wrapped.SetFlags()
	frames := [][]byte{
		authorizationFrame(t, key, 42, 1<<32, &tg.HelpGetConfigRequest{}),
		authorizationFrame(t, key, 42, 2<<32, &tg.InvokeWithLayerRequest{
			Layer: 100,
			Query: wrapped,
		}),
		authorizationContainerFrame(t, key, 42, 3<<32,
			&tg.HelpGetConfigRequest{},
			&tg.HelpGetConfigRequest{},
		),
	}
	conn := &authorizationFrameConn{frames: frames}
	err := server.serveConn(context.Background(), conn, netip.MustParseAddr("203.0.113.7"), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("serveConn error = %v, want scripted EOF", err)
	}
	if got, want := store.lookups, len(frames); got != want {
		t.Fatalf("auth-key reads = %d, want one per frame (%d)", got, want)
	}
}

func TestContainerBindingReadsDatabaseOwnerAndKeepsUnboundSlot(t *testing.T) {
	t.Parallel()

	key := authorizationRebindTestKey()
	store := &authorizationBindingReadKeyStore{key: key}
	var handled int
	handler := UnpackInvoke(HandlerFunc(func(c *Conn, req *Request) error {
		handled++
		switch handled {
		case 1:
			// auth.signUp commits a binding during this frame.
			store.owner = 7
			c.RecordAuthorizationBinding(7)
		case 2:
			// auth.signIn on a 2FA account stages a pending user and clears
			// user_id again before the enclosing container finishes.
			store.owner = 0
		}
		return c.SendResult(req, &tg.BoolTrue{})
	}))
	server := New(exchange.PrivateKey{}, 2, store, handler, nil)
	if err := server.SetMaxConnsPerUnboundKey(1); err != nil {
		t.Fatalf("set unbound-key cap: %v", err)
	}

	frames := [][]byte{
		authorizationContainerFrame(t, key, 42, 1<<32,
			&tg.AuthSignUpRequest{PhoneNumber: "+15550000001", PhoneCodeHash: "signup"},
			&tg.AuthSignInRequest{PhoneNumber: "+15550000001", PhoneCodeHash: "signin"},
		),
		authorizationFrame(t, key, 42, 4<<32, &tg.HelpGetConfigRequest{}),
	}
	conn := &authorizationFrameConn{
		frames: frames,
		before: func(served int) {
			if served != 1 {
				return
			}
			if got, want := store.lookups, 2; got != want {
				t.Errorf("auth-key reads after binding container = %d, want %d", got, want)
			}
			if store.owner != 0 {
				t.Errorf("database owner after pending-user step = %d, want 0", store.owner)
			}
			if got, want := server.unboundKeys.conns[key.ID], 1; got != want {
				t.Errorf("unbound-key slots after binding container = %d, want %d", got, want)
			}
		},
	}
	err := server.serveConn(context.Background(), conn, netip.MustParseAddr("203.0.113.7"), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("serveConn error = %v, want scripted EOF", err)
	}
	if got, want := store.lookups, 3; got != want {
		t.Fatalf("auth-key reads after next non-binding frame = %d, want %d", got, want)
	}
}

func authorizationContainerFrame(t *testing.T, key crypto.AuthKey, sessionID, msgID int64, requests ...bin.Encoder) []byte {
	t.Helper()

	messages := make([]proto.Message, len(requests))
	for i, request := range requests {
		var body bin.Buffer
		if err := request.Encode(&body); err != nil {
			t.Fatalf("encode container request %d: %v", i, err)
		}
		messages[i] = proto.Message{
			ID:    msgID + int64(i),
			SeqNo: i + 1,
			Bytes: body.Len(),
			Body:  body.Copy(),
		}
	}
	return authorizationFrame(t, key, sessionID, msgID, &proto.MessageContainer{Messages: messages})
}

type authorizationBindingReadKeyStore struct {
	key     crypto.AuthKey
	owner   int64
	lookups int
}

func (s *authorizationBindingReadKeyStore) Save(context.Context, crypto.AuthKey) error { return nil }

func (s *authorizationBindingReadKeyStore) Get(context.Context, [8]byte, time.Duration) (crypto.AuthKey, int64, bool, PendingLogin, bool, error) {
	s.lookups++
	return s.key, s.owner, false, PendingLogin{}, true, nil
}

func (*authorizationBindingReadKeyStore) Touch(context.Context, [8]byte) error { return nil }

var _ AuthKeyStore = (*authorizationBindingReadKeyStore)(nil)
