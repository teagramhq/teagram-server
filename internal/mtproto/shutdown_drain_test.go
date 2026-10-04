package mtproto_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
)

type shutdownObservedListener struct {
	net.Listener

	closed chan struct{}
	once   sync.Once
}

func (l *shutdownObservedListener) Close() error {
	var err error
	l.once.Do(func() {
		err = l.Listener.Close()
		close(l.closed)
	})
	return err
}

func TestServeShutdownDrainsActiveRPC(t *testing.T) {
	for _, websocketTransport := range []bool{false, true} {
		name := "tcp"
		if websocketTransport {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			key := rebindTestKey()
			keys := &websocketAuthKeyStore{key: key}
			serveCtx, stopServing := context.WithCancel(context.Background())
			t.Cleanup(stopServing)

			listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			observed := &shutdownObservedListener{Listener: listener, closed: make(chan struct{})}
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
			entered := make(chan *mtproto.Request, 1)
			onlineEvents := make(chan struct{}, 4)
			offline := make(chan struct{}, 1)
			srv := mtproto.New(exchange.PrivateKey{}, 2, keys, mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
				entered <- req
				<-release
				return c.SendResult(req, &tg.BoolTrue{})
			}), nil)
			srv.OnStatusChange(func(_ context.Context, _ int64, online bool) {
				if online {
					onlineEvents <- struct{}{}
				} else {
					offline <- struct{}{}
				}
			})
			serveDone := make(chan error, 1)
			if websocketTransport {
				go func() { serveDone <- srv.ServeWebSocket(serveCtx, observed) }()
			} else {
				go func() { serveDone <- srv.Serve(serveCtx, observed) }()
			}
			serveStopped := false
			t.Cleanup(func() {
				releaseHandler()
				stopServing()
				if serveStopped {
					return
				}
				select {
				case err := <-serveDone:
					serveStopped = true
					if err != nil {
						t.Errorf("serve: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not stop")
				}
			})

			clientCtx, cancelClients := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelClients()
			client, closeClient := dialShutdownTransport(t, clientCtx, observed.Addr().String(), websocketTransport)
			t.Cleanup(closeClient)
			if err := client.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})}); err != nil {
				t.Fatalf("send registration ping: %v", err)
			}
			assertShutdownPong(t, clientCtx, client, key, 1)
			waitShutdownSignal(t, clientCtx, onlineEvents, "active connection registration")
			if err := client.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, key, 42, 2<<32, &tg.HelpGetConfigRequest{})}); err != nil {
				t.Fatalf("send held RPC: %v", err)
			}

			var req *mtproto.Request
			select {
			case req = <-entered:
			case <-clientCtx.Done():
				t.Fatal("held RPC did not reach the handler")
			}

			idleClients := make([]transport.Conn, 2)
			closeIdleClients := make([]func(), 2)
			for i := range idleClients {
				idleClients[i], closeIdleClients[i] = dialShutdownTransport(t, clientCtx, observed.Addr().String(), websocketTransport)
				t.Cleanup(closeIdleClients[i])
				pingID := int64(i + 10)
				if err := idleClients[i].Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, key, int64(50+i), int64(1)<<32, &mt.PingRequest{PingID: pingID})}); err != nil {
					t.Fatalf("send idle registration ping: %v", err)
				}
				assertShutdownPong(t, clientCtx, idleClients[i], key, pingID)
				waitShutdownSignal(t, clientCtx, onlineEvents, "idle connection registration")
			}

			stopServing()
			select {
			case <-observed.closed:
			case <-clientCtx.Done():
				t.Fatal("shutdown did not stop accepting connections")
			}
			if err := req.Ctx.Err(); err != nil {
				releaseHandler()
				t.Fatalf("SIGTERM cancelled an admitted RPC: %v", err)
			}
			registered := srv.Registry().Conns(7)
			if len(registered) != 3 {
				t.Fatalf("registered connections = %d, want active plus two idle", len(registered))
			}
			for _, conn := range registered {
				pushed, err := conn.PushTo(context.Background(), 7, &mt.Pong{PingID: 99}, 0)
				if err != nil || !pushed {
					t.Fatalf("pending push during drain = (%t, %v), want a delivered push", pushed, err)
				}
			}
			assertShutdownPong(t, clientCtx, client, key, 99)
			for _, idle := range idleClients {
				assertShutdownPong(t, clientCtx, idle, key, 99)
			}

			failedDialCtx, stopFailedDial := context.WithTimeout(context.Background(), time.Second)
			if websocketTransport {
				ws, response, err := websocket.Dial(failedDialCtx, "ws://"+observed.Addr().String()+"/apiws", nil)
				if response != nil && response.Body != nil {
					if closeErr := response.Body.Close(); closeErr != nil {
						t.Errorf("close rejected WebSocket response: %v", closeErr)
					}
				}
				if err == nil {
					if closeErr := ws.CloseNow(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
						t.Errorf("close unexpected WebSocket: %v", closeErr)
					}
					stopFailedDial()
					t.Fatal("old WebSocket instance accepted after SIGTERM")
				}
			} else {
				conn, err := (&net.Dialer{}).DialContext(failedDialCtx, "tcp", observed.Addr().String())
				if err == nil {
					if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
						t.Errorf("close unexpected TCP connection: %v", closeErr)
					}
					stopFailedDial()
					t.Fatal("old TCP instance accepted after SIGTERM")
				}
			}
			stopFailedDial()

			type idleClose struct {
				at  time.Time
				err error
			}
			idleClosed := make(chan idleClose, len(idleClients))
			for _, idle := range idleClients {
				go func(conn transport.Conn) {
					var frame bin.Buffer
					err := conn.Recv(clientCtx, &frame)
					idleClosed <- idleClose{at: time.Now(), err: err}
				}(idle)
			}
			// A separate replica keeps accepting and answering while this one drains.
			otherListener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen on draining peer: %v", err)
			}
			otherCtx, stopOther := context.WithCancel(context.Background())
			other := mtproto.New(exchange.PrivateKey{}, 2, keys, nil, nil)
			otherDone := make(chan error, 1)
			otherStopped := false
			t.Cleanup(func() {
				if otherStopped {
					return
				}
				stopOther()
				select {
				case <-otherDone:
				case <-time.After(5 * time.Second):
					t.Error("healthy replica did not stop")
				}
			})
			if websocketTransport {
				go func() { otherDone <- other.ServeWebSocket(otherCtx, otherListener) }()
			} else {
				go func() { otherDone <- other.Serve(otherCtx, otherListener) }()
			}
			otherClient, closeOtherClient := dialShutdownTransport(t, clientCtx, otherListener.Addr().String(), websocketTransport)
			if err := otherClient.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, key, 43, 1<<32, &mt.PingRequest{PingID: 2})}); err != nil {
				t.Fatalf("send ping to healthy replica: %v", err)
			}
			assertShutdownPong(t, clientCtx, otherClient, key, 2)
			closeOtherClient()

			releasedAt := time.Now()
			releaseHandler()
			assertShutdownResult(t, clientCtx, client, key, 2<<32)
			firstClose := <-idleClosed
			secondClose := <-idleClosed
			if firstClose.err == nil || secondClose.err == nil {
				t.Fatalf("idle sockets remained open after drain: first=%v second=%v", firstClose.err, secondClose.err)
			}
			if firstClose.at.Before(releasedAt) || secondClose.at.Before(releasedAt) {
				t.Fatalf("idle socket retired before admitted work was released: first=%s second=%s release=%s", firstClose.at, secondClose.at, releasedAt)
			}
			difference := firstClose.at.Sub(secondClose.at)
			if difference < 0 {
				difference = -difference
			}
			if difference < 500*time.Millisecond {
				t.Fatalf("idle sockets did not close in a stagger: first=%s second=%s", firstClose.at, secondClose.at)
			}
			select {
			case <-offline:
			case <-clientCtx.Done():
				t.Fatal("draining socket did not leave the session registry")
			}
			select {
			case err := <-serveDone:
				serveStopped = true
				if err != nil {
					t.Fatalf("serve after drain: %v", err)
				}
			case <-clientCtx.Done():
				t.Fatal("server did not finish draining the admitted RPC")
			}
			stopOther()
			if err := <-otherDone; err != nil {
				t.Fatalf("healthy replica serve: %v", err)
			}
			otherStopped = true
		})
	}
}

func TestServeShutdownStillEvictsRevokedClient(t *testing.T) {
	for _, websocketTransport := range []bool{false, true} {
		name := "tcp"
		if websocketTransport {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			activeKey := seededKey(21)
			revokedKey := seededKey(22)
			keys := &shutdownAuthKeyStore{keys: map[[8]byte]crypto.AuthKey{
				activeKey.ID:  activeKey,
				revokedKey.ID: revokedKey,
			}}
			serveCtx, stopServing := context.WithCancel(context.Background())
			t.Cleanup(stopServing)

			listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			observed := &shutdownObservedListener{Listener: listener, closed: make(chan struct{})}
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
			entered := make(chan *mtproto.Request, 1)
			srv := mtproto.New(exchange.PrivateKey{}, 2, keys, mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
				entered <- req
				<-release
				return c.SendResult(req, &tg.BoolTrue{})
			}), nil)
			serveDone := make(chan error, 1)
			if websocketTransport {
				go func() { serveDone <- srv.ServeWebSocket(serveCtx, observed) }()
			} else {
				go func() { serveDone <- srv.Serve(serveCtx, observed) }()
			}
			serveStopped := false
			t.Cleanup(func() {
				releaseHandler()
				stopServing()
				if serveStopped {
					return
				}
				select {
				case err := <-serveDone:
					if err != nil {
						t.Errorf("serve: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not stop")
				}
			})

			clientCtx, cancelClients := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelClients()
			activeClient, closeActiveClient := dialShutdownTransport(t, clientCtx, observed.Addr().String(), websocketTransport)
			t.Cleanup(closeActiveClient)
			if err := activeClient.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, activeKey, 42, 1<<32, &mt.PingRequest{PingID: 1})}); err != nil {
				t.Fatalf("send active registration ping: %v", err)
			}
			assertShutdownPong(t, clientCtx, activeClient, activeKey, 1)

			revokedClient, closeRevokedClient := dialShutdownTransport(t, clientCtx, observed.Addr().String(), websocketTransport)
			t.Cleanup(closeRevokedClient)
			if err := revokedClient.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, revokedKey, 43, 1<<32, &mt.PingRequest{PingID: 2})}); err != nil {
				t.Fatalf("send revocable registration ping: %v", err)
			}
			assertShutdownPong(t, clientCtx, revokedClient, revokedKey, 2)

			if err := activeClient.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, activeKey, 42, 2<<32, &tg.HelpGetConfigRequest{})}); err != nil {
				t.Fatalf("send held RPC: %v", err)
			}
			var req *mtproto.Request
			select {
			case req = <-entered:
			case <-clientCtx.Done():
				t.Fatal("held RPC did not reach the handler")
			}

			stopServing()
			select {
			case <-observed.closed:
			case <-clientCtx.Done():
				t.Fatal("shutdown did not stop accepting connections")
			}
			if err := req.Ctx.Err(); err != nil {
				t.Fatalf("SIGTERM cancelled an admitted RPC: %v", err)
			}

			// A second replica keeps accepting while A drains. Its revocation
			// notification is then applied to A's local registry, as the shared
			// notifier does in production.
			otherListener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen on draining peer: %v", err)
			}
			otherCtx, stopOther := context.WithCancel(context.Background())
			other := mtproto.New(exchange.PrivateKey{}, 2, keys, nil, nil)
			otherDone := make(chan error, 1)
			otherStopped := false
			t.Cleanup(func() {
				if otherStopped {
					return
				}
				stopOther()
				select {
				case <-otherDone:
				case <-time.After(5 * time.Second):
					t.Error("healthy replica did not stop")
				}
			})
			if websocketTransport {
				go func() { otherDone <- other.ServeWebSocket(otherCtx, otherListener) }()
			} else {
				go func() { otherDone <- other.Serve(otherCtx, otherListener) }()
			}
			otherClient, closeOtherClient := dialShutdownTransport(t, clientCtx, otherListener.Addr().String(), websocketTransport)
			if err := otherClient.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, activeKey, 44, 1<<32, &mt.PingRequest{PingID: 3})}); err != nil {
				t.Fatalf("send ping to healthy replica: %v", err)
			}
			assertShutdownPong(t, clientCtx, otherClient, activeKey, 3)
			closeOtherClient()

			var victim *mtproto.Conn
			for _, conn := range srv.Registry().Conns(7) {
				if conn.AuthKeyID() == revokedKey.IntID() {
					victim = conn
					break
				}
			}
			if victim == nil {
				t.Fatal("revoked connection is missing from the draining registry")
			}
			keys.revoke(revokedKey.ID)
			updater := api.NewUpdater(nil, srv.Registry(), slog.New(slog.DiscardHandler), nil)
			updater.Evict(context.Background(), 7, revokedKey.IntID())
			if pushed, pushErr := victim.PushTo(context.Background(), 7, &mt.Pong{PingID: 99}, 0); pushed {
				t.Fatalf("revoked connection accepted a push during drain (err=%v)", pushErr)
			}
			if sendErr := revokedClient.Send(clientCtx, &bin.Buffer{Buf: clientFrame(t, revokedKey, 43, 2<<32, &mt.PingRequest{PingID: 4})}); sendErr == nil {
				receiveCtx, stopReceive := context.WithTimeout(clientCtx, time.Second)
				var frame bin.Buffer
				recvErr := revokedClient.Recv(receiveCtx, &frame)
				stopReceive()
				if recvErr == nil {
					t.Fatal("revoked client received a frame from the draining replica")
				}
				if errors.Is(recvErr, context.DeadlineExceeded) {
					t.Fatal("revoked client connection stayed open after the revocation event")
				}
			}

			releaseHandler()
			assertShutdownResult(t, clientCtx, activeClient, activeKey, 2<<32)
			select {
			case err := <-serveDone:
				serveStopped = true
				if err != nil {
					t.Fatalf("serve after drain: %v", err)
				}
			case <-clientCtx.Done():
				t.Fatal("server did not finish draining the admitted RPC")
			}
			stopOther()
			if err := <-otherDone; err != nil {
				t.Fatalf("healthy replica serve: %v", err)
			}
			otherStopped = true
		})
	}
}

func TestServeWebSocketShutdownDoesNotAwaitFirstFrame(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mtproto.New(exchange.PrivateKey{}, 2, mtproto.NewMemoryAuthKeyStore(), nil, nil)
	srv.SetHandshakeTimeout(30 * time.Second)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ServeWebSocket(ctx, listener) }()

	clientCtx, cancelClient := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClient()
	ws, err := dialWebSocket(clientCtx, listener.Addr().String())
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	t.Cleanup(func() { closeWebSocket(t, ws) })
	cancel()

	closeCtx, stopClose := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopClose()
	if _, _, err := ws.Read(closeCtx); err == nil {
		t.Fatal("WebSocket stayed open while the server awaited its first MTProto frame")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("WebSocket stayed open until the client-side deadline after shutdown")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve WebSocket: %v", err)
		}
	case <-closeCtx.Done():
		t.Fatal("ServeWebSocket waited on a connection before its first MTProto frame")
	}
}

type shutdownAuthKeyStore struct {
	mu   sync.RWMutex
	keys map[[8]byte]crypto.AuthKey
}

func (s *shutdownAuthKeyStore) Save(context.Context, crypto.AuthKey) error { return nil }
func (s *shutdownAuthKeyStore) Touch(context.Context, [8]byte) error       { return nil }
func (s *shutdownAuthKeyStore) Get(_ context.Context, id [8]byte) (crypto.AuthKey, int64, bool, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[id]
	return key, 7, false, ok, nil
}

func (s *shutdownAuthKeyStore) revoke(id [8]byte) {
	s.mu.Lock()
	delete(s.keys, id)
	s.mu.Unlock()
}

func waitShutdownSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", description)
	}
}

func dialShutdownTransport(t *testing.T, ctx context.Context, addr string, websocketTransport bool) (transport.Conn, func()) {
	t.Helper()
	if !websocketTransport {
		raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			t.Fatalf("dial TCP: %v", err)
		}
		conn, err := transport.Abridged.Handshake(raw)
		if err != nil {
			if closeErr := raw.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				t.Errorf("close raw TCP connection after handshake failure: %v", closeErr)
			}
			t.Fatalf("TCP transport handshake: %v", err)
		}
		return conn, func() {
			if closeErr := raw.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				t.Errorf("close raw TCP connection: %v", closeErr)
			}
		}
	}
	ws, response, err := websocket.Dial(ctx, "ws://"+addr+"/apiws", nil)
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close WebSocket response body: %v", closeErr)
		}
	}
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	stream := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	conn, err := transport.Intermediate.Handshake(stream)
	if err != nil {
		if closeErr := ws.CloseNow(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("close WebSocket after transport handshake failure: %v", closeErr)
		}
		t.Fatalf("WebSocket transport handshake: %v", err)
	}
	return conn, func() {
		if closeErr := ws.CloseNow(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("close WebSocket: %v", closeErr)
		}
	}
}

func shutdownReceiveBody(ctx context.Context, conn transport.Conn, key crypto.AuthKey, cipher crypto.Cipher) ([]byte, error) {
	var frame bin.Buffer
	if err := conn.Recv(ctx, &frame); err != nil {
		return nil, err
	}
	encrypted := &crypto.EncryptedMessage{}
	if err := encrypted.DecodeWithoutCopy(&frame); err != nil {
		return nil, err
	}
	message, err := cipher.Decrypt(key, encrypted)
	if err != nil {
		return nil, err
	}
	return message.Data(), nil
}

func assertShutdownPong(t *testing.T, ctx context.Context, conn transport.Conn, key crypto.AuthKey, pingID int64) {
	t.Helper()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for {
		body, err := shutdownReceiveBody(ctx, conn, key, cipher)
		if err != nil {
			t.Fatalf("receive ping response: %v", err)
		}
		id, err := (&bin.Buffer{Buf: body}).PeekID()
		if err != nil {
			t.Fatalf("peek ping response: %v", err)
		}
		if id == mt.NewSessionCreatedTypeID {
			continue
		}
		pong := &mt.Pong{}
		if err := pong.Decode(&bin.Buffer{Buf: body}); err != nil {
			t.Fatalf("decode ping response: %v", err)
		}
		if pong.PingID != pingID {
			t.Fatalf("pong ping ID = %d, want %d", pong.PingID, pingID)
		}
		return
	}
}

func assertShutdownResult(t *testing.T, ctx context.Context, conn transport.Conn, key crypto.AuthKey, msgID int64) {
	t.Helper()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for {
		body, err := shutdownReceiveBody(ctx, conn, key, cipher)
		if err != nil {
			t.Fatalf("receive admitted RPC result: %v", err)
		}
		var result proto.Result
		if err := result.Decode(&bin.Buffer{Buf: body}); err != nil || result.RequestMessageID != msgID {
			continue
		}
		answer := &tg.BoolTrue{}
		if err := answer.Decode(&bin.Buffer{Buf: result.Result}); err != nil {
			t.Fatalf("decode admitted RPC result: %v", err)
		}
		return
	}
}
