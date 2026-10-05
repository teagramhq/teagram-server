package mtproto

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

const shutdownPayloadSize = 15 << 20

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

type shutdownPayload struct {
	data  []byte
	ready chan struct{}
}

func (e *shutdownPayload) Encode(b *bin.Buffer) error {
	b.Put(e.data)
	close(e.ready)
	return nil
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
			pair := openShutdownTransportPair(t, server, websocketTransport, nil)
			defer pair.close()
			defer stopShutdownTimers(server.shutdown)

			key := crypto.Key{1, 2, 3, 4}.WithID()
			conn := newConn(pair.server, crypto.NewServerCipher(crypto.DefaultRand()), proto.NewMessageIDGen(clock.System.Now), clock.System, 2*time.Second, nil)
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

			payload := make([]byte, shutdownPayloadSize)
			replyBody := &shutdownPayload{data: payload, ready: make(chan struct{})}
			replyDone := make(chan error, 1)
			go func() {
				replyDone <- conn.SendResult(&Request{Ctx: context.Background(), MsgID: 1 << 32, SessionID: 42}, replyBody)
			}()
			waitShutdownChannel(t, replyBody.ready, "reply queued behind the socket writer")

			pushBody := &shutdownPayload{data: payload, ready: make(chan struct{})}
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
			limit := writeDeadline.Add(750 * time.Millisecond)
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
