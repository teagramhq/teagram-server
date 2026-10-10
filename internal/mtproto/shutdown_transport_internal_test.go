package mtproto

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/clock"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/transport"
)

type shutdownTransportEndpoint struct {
	conn   transport.Conn
	socket *websocket.Conn
	stream net.Conn
	err    error
}

type shutdownTransportPair struct {
	server transport.Conn
	client transport.Conn
	socket *websocket.Conn
	stream net.Conn
	close  func()
}

func openShutdownTransportPair(t *testing.T, server *Server, websocketTransport bool, wrapServerStream func(net.Conn) net.Conn) shutdownTransportPair {
	t.Helper()
	if !websocketTransport {
		listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen TCP: %v", err)
		}
		clientRaw, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", listener.Addr().String())
		if err != nil {
			closeShutdownResource(t, "TCP listener", listener)
			t.Fatalf("dial TCP: %v", err)
		}
		serverRaw, err := listener.Accept()
		if err != nil {
			closeShutdownResource(t, "TCP client socket", clientRaw)
			closeShutdownResource(t, "TCP listener", listener)
			t.Fatalf("accept TCP: %v", err)
		}
		closeShutdownResource(t, "TCP listener", listener)
		client, err := transport.Abridged.Handshake(clientRaw)
		if err != nil {
			closeShutdownResource(t, "TCP client socket", clientRaw)
			closeShutdownResource(t, "TCP server socket", serverRaw)
			t.Fatalf("client TCP transport handshake: %v", err)
		}
		stream := serverRaw
		if wrapServerStream != nil {
			stream = wrapServerStream(stream)
		}
		serverConn, err := server.detectCodec(stream)
		if err != nil {
			closeShutdownResource(t, "TCP transport client", client)
			t.Fatalf("server TCP transport detection: %v", err)
		}
		return shutdownTransportPair{
			server: serverConn,
			client: client,
			stream: stream,
			close: func() {
				closeShutdownResource(t, "TCP server transport", serverConn)
				closeShutdownResource(t, "TCP client transport", client)
			},
		}
	}

	ready := make(chan shutdownTransportEndpoint, 1)
	stopHandler := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			ready <- shutdownTransportEndpoint{err: err}
			return
		}
		stream := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		if wrapServerStream != nil {
			stream = wrapServerStream(stream)
		}
		serverConn, err := server.detectCodec(stream)
		ready <- shutdownTransportEndpoint{conn: serverConn, socket: ws, stream: stream, err: err}
		<-stopHandler
		checkShutdownClose(t, "server WebSocket", ws.CloseNow())
	}))
	wsURL := "ws" + httpServer.URL[len("http"):]
	wsClient, response, err := websocket.Dial(context.Background(), wsURL, nil)
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close WebSocket response body: %v", closeErr)
		}
	}
	if err != nil {
		close(stopHandler)
		httpServer.Close()
		t.Fatalf("dial WebSocket: %v", err)
	}
	clientStream := websocket.NetConn(context.Background(), wsClient, websocket.MessageBinary)
	client, err := transport.Intermediate.Handshake(clientStream)
	if err != nil {
		close(stopHandler)
		checkShutdownClose(t, "client WebSocket", wsClient.CloseNow())
		httpServer.Close()
		t.Fatalf("client WebSocket transport handshake: %v", err)
	}
	var endpoint shutdownTransportEndpoint
	select {
	case endpoint = <-ready:
	case <-time.After(5 * time.Second):
		close(stopHandler)
		closeShutdownResource(t, "client transport", client)
		httpServer.Close()
		t.Fatal("server did not detect WebSocket transport")
	}
	if endpoint.err != nil {
		close(stopHandler)
		closeShutdownResource(t, "client transport", client)
		httpServer.Close()
		t.Fatalf("server WebSocket transport detection: %v", endpoint.err)
	}
	return shutdownTransportPair{
		server: endpoint.conn,
		client: client,
		socket: endpoint.socket,
		stream: endpoint.stream,
		close: func() {
			close(stopHandler)
			checkShutdownClose(t, "client WebSocket", wsClient.CloseNow())
			checkShutdownClose(t, "server WebSocket", endpoint.socket.CloseNow())
			httpServer.Close()
		},
	}
}

type deadlineObservedConn struct {
	net.Conn

	deadlines chan time.Time
}

type shutdownTestCloser interface {
	Close() error
}

func closeShutdownResource(t *testing.T, name string, closer shutdownTestCloser) {
	t.Helper()
	checkShutdownClose(t, name, closer.Close())
}

func checkShutdownClose(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close %s: %v", name, err)
	}
}

func (c *deadlineObservedConn) SetReadDeadline(deadline time.Time) error {
	select {
	case c.deadlines <- deadline:
	default:
	}
	return c.Conn.SetReadDeadline(deadline)
}

// A native pipe with no reader supplies backpressure without allocating or
// encrypting a large payload. Reads and negotiation still use the real stream.
type shutdownStalledWriteConn struct {
	net.Conn

	writer    net.Conn
	deadlines chan time.Time
}

func (c *shutdownStalledWriteConn) Write(p []byte) (int, error) {
	return c.writer.Write(p)
}

func (c *shutdownStalledWriteConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.deadlines <- deadline
	}
	return errors.Join(c.Conn.SetWriteDeadline(deadline), c.writer.SetWriteDeadline(deadline))
}

func (c *shutdownStalledWriteConn) Close() error {
	return errors.Join(c.Conn.Close(), c.writer.Close())
}

func TestQueuedReplyAndPushUseDrainDeadlineOnRealTransports(t *testing.T) {
	for _, websocketTransport := range []bool{false, true} {
		name := "TCP"
		if websocketTransport {
			name = "WebSocket"
		}
		t.Run(name, func(t *testing.T) {
			server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, nil)
			server.shutdown.drainTimeout = 800 * time.Millisecond
			server.shutdown.retirementWindow = 200 * time.Millisecond
			writer, unread := net.Pipe()
			defer closeShutdownResource(t, "stalled pipe writer", writer)
			defer closeShutdownResource(t, "stalled pipe reader", unread)
			deadlines := make(chan time.Time, 4)
			pair := openShutdownTransportPair(t, server, websocketTransport, func(stream net.Conn) net.Conn {
				return &shutdownStalledWriteConn{Conn: stream, writer: writer, deadlines: deadlines}
			})
			defer pair.close()
			defer stopShutdownTimers(server.shutdown)

			key := crypto.Key{1, 2, 3, 4}.WithID()
			serverTransport := pair.server
			// Match ServeWebSocket: a failed push must not hold writeMu
			// through a normal WebSocket close handshake during drain.
			if websocketTransport {
				serverTransport = webSocketDrainConn{Conn: pair.server, socket: pair.socket, shutdown: server.shutdown}
			}
			conn := newConn(serverTransport, crypto.NewServerCipher(crypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, 2*time.Second, nil)
			conn.shutdown = server.shutdown
			conn.setKey(key)
			conn.setSession(42)
			conn.setOwner(7)

			conn.writeMu.Lock()
			locked := true
			defer func() {
				if locked {
					conn.writeMu.Unlock()
				}
			}()

			replyBody := &shutdownReadyPong{pingID: 1, ready: make(chan struct{})}
			replyDone := make(chan error, 1)
			go func() {
				replyDone <- conn.SendResult(&Request{Ctx: context.Background(), MsgID: 1 << 32, SessionID: 42}, replyBody)
			}()
			waitShutdownChannel(t, replyBody.ready, "reply queued behind the socket writer")

			pushBody := &shutdownReadyPong{pingID: 2, ready: make(chan struct{})}
			pushDone := make(chan struct {
				pushed bool
				err    error
			}, 1)
			go func() {
				pushed, err := conn.PushTo(context.Background(), 7, pushBody, 0)
				pushDone <- struct {
					pushed bool
					err    error
				}{pushed: pushed, err: err}
			}()
			waitShutdownChannel(t, pushBody.ready, "push queued behind the socket writer")

			server.shutdown.beginDrain()
			time.Sleep(100 * time.Millisecond)
			conn.writeMu.Unlock()
			locked = false

			writeDeadline := time.Unix(0, server.shutdown.writeDeadline.Load())
			// Check the deadline applied by the real transport, independently of
			// when a loaded runner schedules the goroutines that return errors.
			select {
			case applied := <-deadlines:
				if !applied.Equal(writeDeadline) {
					t.Fatalf("socket write deadline = %s, want shared drain deadline %s", applied, writeDeadline)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("queued output did not reach the socket writer")
			}
			limit := writeDeadline.Add(5 * time.Second)
			select {
			case err := <-replyDone:
				if err == nil {
					t.Fatal("queued reply succeeded for a peer that is not reading")
				}
			case <-time.After(time.Until(limit)):
				t.Fatal("queued reply outlived the shared drain write deadline")
			}
			select {
			case result := <-pushDone:
				if result.pushed || result.err == nil {
					t.Fatalf("queued push = (%t, %v), want bounded failure", result.pushed, result.err)
				}
			case <-time.After(time.Until(limit)):
				t.Fatal("queued push outlived the shared drain write deadline")
			}
		})
	}
}

func TestWebSocketDrainKeepsPendingReadPastItsDeadline(t *testing.T) {
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, nil)
	server.readTimeout = 150 * time.Millisecond
	server.shutdown.drainTimeout = time.Second
	server.shutdown.retirementWindow = 200 * time.Millisecond
	deadlines := make(chan time.Time, 4)
	var readDeadlineConn *webSocketDrainReadDeadlineConn
	pair := openShutdownTransportPair(t, server, true, func(stream net.Conn) net.Conn {
		observed := &deadlineObservedConn{Conn: stream, deadlines: deadlines}
		readDeadlineConn = &webSocketDrainReadDeadlineConn{Conn: observed}
		return readDeadlineConn
	})
	defer pair.close()
	defer stopShutdownTimers(server.shutdown)

	drainingConn := webSocketDrainConn{Conn: pair.server, socket: pair.socket, shutdown: server.shutdown, readDeadline: readDeadlineConn}
	key := crypto.Key{1, 2, 3, 4}.WithID()
	conn := newConn(drainingConn, crypto.NewServerCipher(crypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, time.Second, nil)
	conn.shutdown = server.shutdown
	conn.setKey(key)
	conn.setSession(42)
	conn.setOwner(7)
	releaseRPC, admitted := server.shutdown.admitRPC()
	if !admitted {
		t.Fatal("test RPC was not admitted before drain")
	}
	released := false
	defer func() {
		if !released {
			releaseRPC()
		}
	}()

	readDone := make(chan error, 1)
	go func() {
		readDone <- server.readOrDrain(server.shutdown.requestCtx, drainingConn, &bin.Buffer{}, time.Time{})
	}()
	var originalDeadline time.Time
	for originalDeadline.IsZero() {
		select {
		case originalDeadline = <-deadlines:
		case <-time.After(time.Second):
			t.Fatal("server did not begin its WebSocket read")
		}
	}

	server.shutdown.beginDrain()
	select {
	case err := <-readDone:
		if !errors.Is(err, errServerDraining) {
			t.Fatalf("readOrDrain = %v, want drain signal", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read loop did not observe drain")
	}
	if wait := time.Until(originalDeadline.Add(50 * time.Millisecond)); wait > 0 {
		timer := time.NewTimer(wait)
		<-timer.C
		timer.Stop()
	}

	pushed, err := conn.PushTo(context.Background(), 7, &mt.Pong{PingID: 99}, 0)
	if err != nil || !pushed {
		t.Fatalf("push after the original read deadline = (%t, %v), want delivery", pushed, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var frame bin.Buffer
	if err := pair.client.Recv(ctx, &frame); err != nil {
		t.Fatalf("receive WebSocket push after the original read deadline: %v", err)
	}
	encrypted := &crypto.EncryptedMessage{}
	if err := encrypted.DecodeWithoutCopy(&frame); err != nil {
		t.Fatalf("decode WebSocket push: %v", err)
	}
	message, err := crypto.NewClientCipher(crypto.DefaultRand()).Decrypt(key, encrypted)
	if err != nil {
		t.Fatalf("decrypt WebSocket push: %v", err)
	}
	pong := &mt.Pong{}
	if err := pong.Decode(&bin.Buffer{Buf: message.Data()}); err != nil || pong.PingID != 99 {
		t.Fatalf("WebSocket push = (%+v, %v), want ping 99", pong, err)
	}
	releaseRPC()
	released = true
}

type shutdownLookupAuthKeyStore struct {
	key        crypto.AuthKey
	lookups    atomic.Int32
	blocked    chan struct{}
	release    chan struct{}
	releaseOne sync.Once
}

func (s *shutdownLookupAuthKeyStore) Save(context.Context, crypto.AuthKey) error { return nil }
func (s *shutdownLookupAuthKeyStore) Touch(context.Context, [8]byte) error       { return nil }

func (s *shutdownLookupAuthKeyStore) Get(_ context.Context, id [8]byte, _ time.Duration) (crypto.AuthKey, int64, bool, PendingLogin, bool, error) {
	if id != s.key.ID {
		return crypto.AuthKey{}, 0, false, PendingLogin{}, false, nil
	}
	if s.lookups.Add(1) == 2 {
		close(s.blocked)
		<-s.release
	}
	return s.key, 7, false, PendingLogin{}, true, nil
}

func (s *shutdownLookupAuthKeyStore) unblock() {
	s.releaseOne.Do(func() { close(s.release) })
}

type shutdownReadyPong struct {
	pingID int64
	ready  chan struct{}
}

func (e *shutdownReadyPong) Encode(b *bin.Buffer) error {
	defer close(e.ready)
	return (&mt.Pong{PingID: e.pingID}).Encode(b)
}

func TestWebSocketDrainDuringKeyLookupWaitsForQueuedPush(t *testing.T) {
	key := shutdownTestAuthKey()
	keys := &shutdownLookupAuthKeyStore{
		key:     key,
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
	server := New(exchange.PrivateKey{}, 2, keys, nil, nil)
	server.shutdown.drainTimeout = 2 * time.Second
	server.shutdown.retirementWindow = 500 * time.Millisecond
	pair := openShutdownTransportPair(t, server, true, nil)
	pairClosed := false
	defer func() {
		if !pairClosed {
			pair.close()
		}
	}()
	defer stopShutdownTimers(server.shutdown)

	networkListener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for WebSocket drain state: %v", err)
	}
	listener := &webSocketListener{
		Listener: networkListener,
		done:     make(chan struct{}),
		pending:  make(map[*webSocketAcceptedConn]struct{}),
	}
	accepted := &webSocketAcceptedConn{listener: listener}
	listener.pending[accepted] = struct{}{}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close WebSocket listener: %v", err)
		}
		keys.unblock()
	}()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.serveConnWithContexts(
			server.shutdown.requestCtx,
			server.shutdown.requestCtx,
			pair.server,
			netip.Addr{},
			nil,
			accepted.markServing,
		)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pair.client.Send(ctx, &bin.Buffer{Buf: shutdownClientFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})}); err != nil {
		t.Fatalf("send initial WebSocket ping: %v", err)
	}
	assertShutdownInternalPong(t, ctx, pair.client, key, 1)
	var connections []*Conn
	registryTicker := time.NewTicker(time.Millisecond)
	defer registryTicker.Stop()
	for {
		connections = server.Registry().Conns(7)
		if len(connections) == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("registered WebSocket connections = %d, want 1", len(connections))
		case <-registryTicker.C:
		}
	}
	conn := connections[0]
	conn.writeMu.Lock()
	writeLocked := true
	defer func() {
		if writeLocked {
			conn.writeMu.Unlock()
		}
	}()

	queuedPong := &shutdownReadyPong{pingID: 99, ready: make(chan struct{})}
	pushDone := make(chan struct {
		pushed bool
		err    error
	}, 1)
	go func() {
		pushed, err := conn.PushTo(context.Background(), 7, queuedPong, 0)
		pushDone <- struct {
			pushed bool
			err    error
		}{pushed: pushed, err: err}
	}()
	waitShutdownChannel(t, queuedPong.ready, "WebSocket push queued behind the writer lock")
	if err := pair.client.Send(ctx, &bin.Buffer{Buf: shutdownClientFrame(t, key, 42, 2<<32, &mt.MsgsAck{MsgIDs: []int64{1 << 32}})}); err != nil {
		t.Fatalf("send acknowledgement entering auth-key lookup: %v", err)
	}
	waitShutdownChannel(t, keys.blocked, "second WebSocket auth-key lookup")

	server.shutdown.beginDrain()
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close WebSocket listener during drain: %v", err)
	}
	keys.unblock()
	conn.writeMu.Unlock()
	writeLocked = false

	closed := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer closed.Stop()
	defer ticker.Stop()
	var serveReturned bool
	var serveResult error
waitPushAdmission:
	for conn.pushState.Load()&pushAdmissionClosed == 0 {
		select {
		case err := <-serveDone:
			if conn.pushState.Load()&pushAdmissionClosed != 0 {
				serveReturned = true
				serveResult = err
				break waitPushAdmission
			}
			t.Fatalf("WebSocket handler returned before retiring its queued push: %v", err)
		case <-closed.C:
			t.Fatal("WebSocket handler did not begin retiring after key lookup returned")
		case <-ticker.C:
		}
	}
	select {
	case result := <-pushDone:
		if !result.pushed || result.err != nil {
			t.Fatalf("queued WebSocket push = (%t, %v), want success", result.pushed, result.err)
		}
	case <-ctx.Done():
		t.Fatal("queued WebSocket push did not finish")
	}
	assertShutdownInternalPong(t, ctx, pair.client, key, 99)
	pair.close()
	pairClosed = true
	if serveReturned {
		if serveResult != nil {
			t.Fatalf("serve WebSocket connection after drain: %v", serveResult)
		}
	} else {
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatalf("serve WebSocket connection after drain: %v", err)
			}
		case <-ctx.Done():
			t.Fatalf("WebSocket handler did not return after its queued push (serve results=%d, push state=%#x)", len(serveDone), conn.pushState.Load())
		}
	}
}

func shutdownTestAuthKey() crypto.AuthKey {
	var raw crypto.Key
	for i := range raw {
		raw[i] = byte(i)
	}
	return raw.WithID()
}

func shutdownClientFrame(t *testing.T, key crypto.AuthKey, sessionID, msgID int64, body bin.Encoder) []byte {
	t.Helper()
	var b bin.Buffer
	if err := body.Encode(&b); err != nil {
		t.Fatalf("encode client frame: %v", err)
	}
	data := crypto.EncryptedMessageData{
		SessionID:              sessionID,
		MessageID:              msgID,
		MessageDataLen:         int32(b.Len()), //nolint:gosec // Small test ping frame.
		MessageDataWithPadding: b.Copy(),
	}
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &b); err != nil {
		t.Fatalf("encrypt client frame: %v", err)
	}
	return b.Copy()
}

func assertShutdownInternalPong(t *testing.T, ctx context.Context, conn transport.Conn, key crypto.AuthKey, pingID int64) {
	t.Helper()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for {
		var frame bin.Buffer
		if err := conn.Recv(ctx, &frame); err != nil {
			t.Fatalf("receive WebSocket pong: %v", err)
		}
		encrypted := &crypto.EncryptedMessage{}
		if err := encrypted.DecodeWithoutCopy(&frame); err != nil {
			t.Fatalf("decode WebSocket response: %v", err)
		}
		message, err := cipher.Decrypt(key, encrypted)
		if err != nil {
			t.Fatalf("decrypt WebSocket response: %v", err)
		}
		body := &bin.Buffer{Buf: message.Data()}
		id, err := body.PeekID()
		if err != nil {
			t.Fatalf("peek WebSocket response: %v", err)
		}
		if id == mt.NewSessionCreatedTypeID {
			continue
		}
		pong := &mt.Pong{}
		if err := pong.Decode(body); err != nil {
			t.Fatalf("decode WebSocket pong: %v", err)
		}
		if pong.PingID != pingID {
			t.Fatalf("WebSocket pong ping ID = %d, want %d", pong.PingID, pingID)
		}
		return
	}
}

func waitShutdownChannel(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func stopShutdownTimers(shutdown *serverShutdown) {
	if timer := shutdown.outputTimer.Load(); timer != nil {
		timer.Stop()
	}
	if timer := shutdown.drainTimer.Load(); timer != nil {
		timer.Stop()
	}
	shutdown.cancelOutput()
	shutdown.cancelReq()
}
