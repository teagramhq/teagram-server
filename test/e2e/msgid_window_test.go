package e2e_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/exchange"
	gotdmtproto "github.com/gotd/td/mtproto"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

type msgIDWindowServer struct {
	port     int
	key      *rsa.PrivateKey
	executed atomic.Int64
}

func startMsgIDWindowServer(t *testing.T, ctx context.Context) *msgIDWindowServer {
	t.Helper()
	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("store close: %v", cerr)
		}
	})
	const dcID = 2
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	handler := api.New(st, dcID, fixtureConfigForListener(t, dcID, ln), slog.New(slog.DiscardHandler), false, 100<<20, blobs, 2<<30, pgtest.PeerDeriver(), pgtest.PhotoDeriver(), config.RateLimitsConfig{}, config.RegistrationClosed)
	s := &msgIDWindowServer{key: key}
	counting := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		s.executed.Add(1)
		return handler.OnMessage(c, req)
	})
	server := mtproto.New(exchange.PrivateKey{RSA: key}, dcID, mtproto.NewPgAuthKeyStore(st), counting, nil)
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr type = %T", ln.Addr())
	}
	s.port = addr.Port

	srvCtx, srvCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(srvCtx, ln) }()
	t.Cleanup(func() {
		srvCancel()
		if serr := <-serveErr; serr != nil && !errors.Is(serr, context.Canceled) {
			t.Errorf("server serve: %v", serr)
		}
	})
	return s
}

var errInvokeDone = errors.New("invoke done")

// invoke runs one help.getNearestDc on a fresh MTProto connection whose msg_ids
// are dated offset from the server's time. Only the ids are skewed: gotd drops
// any server message outside its own window, so a client clock 10 minutes
// slow would also discard the bad_msg_notification it is sent. It drives
// gotd's MTProto layer directly because the full client retries a failed
// connection setup until its context ends.
func (s *msgIDWindowServer) invoke(ctx context.Context, offset time.Duration) (*tg.NearestDC, error) {
	const dcID = 2
	list := dcs.List{Options: []tg.DCOption{{ID: dcID, IPAddress: "127.0.0.1", Port: s.port}}}
	resolver := dcs.Plain(dcs.PlainOptions{})
	conn := gotdmtproto.New(func(ctx context.Context) (transport.Conn, error) {
		return resolver.Primary(ctx, dcID, list)
	}, gotdmtproto.Options{
		DC:         dcID,
		PublicKeys: []exchange.PublicKey{{RSA: &s.key.PublicKey}},
		MessageID:  proto.NewMessageIDGen(func() time.Time { return time.Now().Add(offset) }),
	})
	var nearest tg.NearestDC
	// Run serves until its callback fails, so a successful call ends it with
	// errInvokeDone.
	err := conn.Run(ctx, func(ctx context.Context) error {
		if err := conn.Invoke(ctx, &tg.HelpGetNearestDCRequest{}, &nearest); err != nil {
			return err
		}
		return errInvokeDone
	})
	if !errors.Is(err, errInvokeDone) {
		return nil, err
	}
	return &nearest, nil
}

// TestSkewedClientMsgIDIsRefusedUnexecuted proves a client whose clock puts its
// msg_ids outside the server's time window gets bad_msg_notification 16 or 17
// and has nothing executed, while a client on the server's time is served.
func TestSkewedClientMsgIDIsRefusedUnexecuted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		offset time.Duration
		want   string
	}{
		{name: "behind", offset: -10 * time.Minute, want: "msg_id too low"},
		{name: "ahead", offset: 2 * time.Minute, want: "msg_id too high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s := startMsgIDWindowServer(t, ctx)

			if _, err := s.invoke(ctx, tc.offset); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("skewed client invoke = %v, want %q", err, tc.want)
			}
			if n := s.executed.Load(); n != 0 {
				t.Fatalf("server executed %d requests from the skewed client, want none", n)
			}

			nearest, err := s.invoke(ctx, 0)
			if err != nil {
				t.Fatalf("client on server time: %v", err)
			}
			if nearest.ThisDC != 2 {
				t.Fatalf("help.getNearestDc this_dc = %d, want 2", nearest.ThisDC)
			}
			if n := s.executed.Load(); n != 1 {
				t.Fatalf("server executed %d requests for the client on server time, want 1", n)
			}
		})
	}
}
