package mtproto

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/discovery"
)

func TestServeSocketLogsCloseBeforeCodecDetection(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})

	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	serverSocket, err := listener.Accept()
	if err != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("close client after accept failure: %v", closeErr)
		}
		t.Fatalf("accept: %v", err)
	}
	slot, ok := server.preAuth.admit()
	if !ok {
		t.Fatal("pre-auth slot unavailable")
	}
	served := make(chan struct{})
	go func() {
		server.serveSocket(serverSocket, slot)
		close(served)
	}()
	if _, err := client.Write([]byte{0x01}); err != nil {
		t.Fatalf("write non-discovery prefix: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("serveSocket did not return after EOF during codec detection")
	}

	assertSingleTransportCloseRecord(t, sink.records, "TCP", "none", "client close")
}

func TestServeSocketLogsPreAuthDrainAsServerClose(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	t.Cleanup(func() { stopShutdownTimers(server.shutdown) })

	client, serverSocket := net.Pipe()
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close client: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := serverSocket.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close server socket: %v", err)
		}
	})
	slot, ok := server.preAuth.admit()
	if !ok {
		t.Fatal("pre-auth slot unavailable")
	}
	served := make(chan struct{})
	go func() {
		server.serveSocket(serverSocket, slot)
		close(served)
	}()
	server.shutdown.beginDrain()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var records []serverErrorLogRecord
	servedCh := (<-chan struct{})(served)
	closeLogged := false
	for servedCh != nil || !closeLogged {
		select {
		case <-servedCh:
			servedCh = nil
		case record := <-sink.events:
			records = append(records, record)
			if record.message == "transport connection closed" {
				closeLogged = true
			}
		case <-ctx.Done():
			t.Fatal("serveSocket did not finish the pre-auth drain")
		}
	}
	assertSingleTransportCloseRecord(t, records, "TCP", "none", "server close")
}

func TestServeSocketThrottlesPreAuthCloseLogs(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	const connections = 8
	for range connections {
		client, serverSocket := net.Pipe()
		if err := client.Close(); err != nil {
			t.Fatalf("close client: %v", err)
		}
		slot, ok := server.preAuth.admit()
		if !ok {
			t.Fatal("pre-auth slot unavailable")
		}
		server.serveSocket(serverSocket, slot)
	}

	var closeRecords []serverErrorLogRecord
	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			closeRecords = append(closeRecords, record)
		}
	}
	if len(closeRecords) != 1 {
		t.Fatalf("transport close records = %d, want one after %d closed connections: %+v", len(closeRecords), connections, sink.records)
	}
	assertTransportCloseRecord(t, closeRecords[0], "TCP", "none", "client close")
	server.preAuthCloseLog.last.Store(time.Now().Add(-preAuthLogInterval).UnixNano())
	client, serverSocket := net.Pipe()
	if err := client.Close(); err != nil {
		t.Fatalf("close client after sampled interval: %v", err)
	}
	slot, ok := server.preAuth.admit()
	if !ok {
		t.Fatal("pre-auth slot unavailable after sampled interval")
	}
	server.serveSocket(serverSocket, slot)

	closeRecords = closeRecords[:0]
	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			closeRecords = append(closeRecords, record)
		}
	}
	if len(closeRecords) != 2 {
		t.Fatalf("transport close records after sampled interval = %d, want two: %+v", len(closeRecords), sink.records)
	}
	if got := closeRecords[1].attrs["suppressed"]; got != int64(connections-1) {
		t.Errorf("suppressed close records = %v, want %d", got, connections-1)
	}
}

func TestServeSocketSkipsTransportCloseLogOnPerNetworkRefusal(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	if err := server.SetPreAuthLimits(PreAuthLimits{MaxConns: 2, MaxConnsPerNet: 1}); err != nil {
		t.Fatalf("set pre-auth limits: %v", err)
	}

	heldSlot, ok := server.preAuth.admit()
	if !ok {
		t.Fatal("first pre-auth slot unavailable")
	}
	if !heldSlot.keyAddr(netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("first connection was refused at the per-network cap")
	}
	t.Cleanup(heldSlot.clear)

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})
	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close client: %v", err)
		}
	})
	serverSocket, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	slot, ok := server.preAuth.admit()
	if !ok {
		t.Fatal("second pre-auth slot unavailable")
	}
	server.serveSocket(serverSocket, slot)

	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			t.Fatalf("per-network refusal emitted a transport-close record: %+v", record)
		}
	}
}

func TestServeSocketLogsPreAuthLifetimeExpiryAsTimeout(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	const lifetime = time.Second
	if err := server.SetPreAuthLimits(PreAuthLimits{Lifetime: lifetime}); err != nil {
		t.Fatalf("set pre-auth lifetime: %v", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})

	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close client: %v", err)
		}
	})
	serverSocket, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() {
		if err := serverSocket.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close server socket: %v", err)
		}
	})
	slot, ok := server.preAuth.admit()
	if !ok {
		t.Fatal("pre-auth slot unavailable")
	}
	served := make(chan struct{})
	go func() {
		server.serveSocket(serverSocket, slot)
		close(served)
	}()

	conn, err := transport.Abridged.Handshake(client)
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*lifetime)
	defer cancel()
	if err := conn.Send(ctx, &bin.Buffer{Buf: exchangeRequestFrame(t)}); err != nil {
		t.Fatalf("send req_pq_multi: %v", err)
	}
	var response bin.Buffer
	if err := conn.Recv(ctx, &response); err != nil {
		t.Fatalf("read resPQ: %v", err)
	}

	var records []serverErrorLogRecord
	servedCh := (<-chan struct{})(served)
	servedReturned, ceilingLogged, closeLogged := false, false, false
	for !servedReturned || !ceilingLogged || !closeLogged {
		select {
		case <-servedCh:
			servedReturned = true
			servedCh = nil
		case record := <-sink.events:
			records = append(records, record)
			if record.message == "connection closed at the pre-auth lifetime ceiling" {
				ceilingLogged = true
			}
			if record.message == "transport connection closed" {
				closeLogged = true
			}
		case <-ctx.Done():
			t.Fatal("serveSocket did not finish the pre-auth lifetime close")
		}
	}
	assertSingleTransportCloseRecord(t, records, "TCP", "resPQ sent", "timeout")
}

func TestServeWebSocketLogsCloseBeforeCodecDetection(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(ctx, listener) }()
	serverReturned := false
	t.Cleanup(func() {
		cancel()
		if serverReturned {
			return
		}
		select {
		case err := <-served:
			serverReturned = true
			if err != nil {
				t.Errorf("serve WebSocket: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("ServeWebSocket did not stop")
		}
	})

	dialCtx, stopDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopDial()
	ws, response, err := websocket.Dial(dialCtx, "ws://"+listener.Addr().String()+"/apiws", &websocket.DialOptions{
		Subprotocols: []string{"binary"},
	})
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close WebSocket response body: %v", closeErr)
		}
	}
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	if err := ws.CloseNow(); err != nil {
		t.Fatalf("close WebSocket before codec detection: %v", err)
	}

	var closeRecord *serverErrorLogRecord
	deadline := time.After(3 * time.Second)
	for closeRecord == nil {
		select {
		case record := <-sink.events:
			if record.message == "transport connection closed" {
				closeRecord = &record
			}
		case <-deadline:
			t.Fatal("no transport close log after WebSocket EOF during codec detection")
		}
	}
	assertTransportCloseRecord(t, *closeRecord, "/apiws", "none", "client close")

	cancel()
	select {
	case err := <-served:
		serverReturned = true
		if err != nil {
			t.Fatalf("serve WebSocket: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeWebSocket did not stop after the client closed")
	}
	closeRecords := []serverErrorLogRecord{*closeRecord}
	for {
		select {
		case record := <-sink.events:
			if record.message == "transport connection closed" {
				closeRecords = append(closeRecords, record)
			}
		default:
			if len(closeRecords) != 1 {
				t.Fatalf("transport close records = %d, want one: %+v", len(closeRecords), closeRecords)
			}
			return
		}
	}
}

func TestServeWebSocketLogsDrainAsServerCloseBeforeCodecDetection(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(ctx, listener) }()
	serverReturned := false
	t.Cleanup(func() {
		cancel()
		if serverReturned {
			return
		}
		select {
		case err := <-served:
			serverReturned = true
			if err != nil {
				t.Errorf("serve WebSocket: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("ServeWebSocket did not stop")
		}
	})

	dialCtx, stopDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopDial()
	ws, response, err := websocket.Dial(dialCtx, "ws://"+listener.Addr().String()+"/apiws", &websocket.DialOptions{
		Subprotocols: []string{"binary"},
	})
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close WebSocket response body: %v", closeErr)
		}
	}
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	t.Cleanup(func() {
		if err := ws.CloseNow(); err != nil && !isDisconnect(err) {
			t.Errorf("close WebSocket client: %v", err)
		}
	})

	server.shutdown.beginDrain()
	waitCtx, stopWait := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopWait()
	var records []serverErrorLogRecord
	servedCh := (<-chan error)(served)
	servedReturned, closeLogged := false, false
	for !servedReturned || !closeLogged {
		select {
		case err := <-servedCh:
			servedReturned = true
			serverReturned = true
			servedCh = nil
			if err != nil {
				t.Fatalf("serve WebSocket: %v", err)
			}
		case record := <-sink.events:
			records = append(records, record)
			if record.message == "transport connection closed" {
				closeLogged = true
			}
		case <-waitCtx.Done():
			t.Fatal("ServeWebSocket did not finish the pre-auth drain")
		}
	}
	assertSingleTransportCloseRecord(t, records, "/apiws", "none", "server close")
}

func TestServeWebSocketLogsPreAuthLifetimeExpiryAsTimeout(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	const lifetime = time.Second
	if err := server.SetPreAuthLimits(PreAuthLimits{Lifetime: lifetime}); err != nil {
		t.Fatalf("set pre-auth lifetime: %v", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(ctx, listener) }()
	serverReturned := false
	t.Cleanup(func() {
		cancel()
		if serverReturned {
			return
		}
		select {
		case err := <-served:
			serverReturned = true
			if err != nil {
				t.Errorf("serve WebSocket: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("ServeWebSocket did not stop")
		}
	})

	dialCtx, stopDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopDial()
	ws, response, err := websocket.Dial(dialCtx, "ws://"+listener.Addr().String()+"/apiws", &websocket.DialOptions{
		Subprotocols: []string{"binary"},
	})
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close WebSocket response body: %v", closeErr)
		}
	}
	if err != nil {
		t.Fatalf("dial WebSocket: %v", err)
	}
	t.Cleanup(func() {
		if err := ws.CloseNow(); err != nil && !isDisconnect(err) {
			t.Errorf("close WebSocket client: %v", err)
		}
	})
	clientStream := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	conn, err := transport.Abridged.Handshake(clientStream)
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(t.Context(), 5*lifetime)
	defer cancelHandshake()
	if err := conn.Send(handshakeCtx, &bin.Buffer{Buf: exchangeRequestFrame(t)}); err != nil {
		t.Fatalf("send req_pq_multi: %v", err)
	}
	var responseFrame bin.Buffer
	if err := conn.Recv(handshakeCtx, &responseFrame); err != nil {
		t.Fatalf("read resPQ: %v", err)
	}

	var records []serverErrorLogRecord
	ceilingLogged, closeLogged := false, false
	for !ceilingLogged || !closeLogged {
		select {
		case record := <-sink.events:
			records = append(records, record)
			if record.message == "WebSocket connection closed at the pre-auth lifetime ceiling" {
				ceilingLogged = true
			}
			if record.message == "transport connection closed" {
				closeLogged = true
			}
		case <-handshakeCtx.Done():
			t.Fatal("WebSocket did not finish the pre-auth lifetime close")
		}
	}
	cancel()
	select {
	case err := <-served:
		serverReturned = true
		if err != nil {
			t.Fatalf("serve WebSocket: %v", err)
		}
	case <-handshakeCtx.Done():
		t.Fatal("ServeWebSocket did not stop after the pre-auth lifetime close")
	}
	for {
		select {
		case record := <-sink.events:
			records = append(records, record)
		default:
			assertSingleTransportCloseRecord(t, records, "/apiws", "resPQ sent", "timeout")
			return
		}
	}
}

func TestServeSocketSkipsTransportCloseLogForDiscoveryPreflight(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})

	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close client: %v", err)
		}
	})
	serverSocket, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	slot, ok := server.preAuth.admit()
	if !ok {
		t.Fatal("pre-auth slot unavailable")
	}
	served := make(chan struct{})
	go func() {
		server.serveSocket(serverSocket, slot)
		close(served)
	}()

	nonce := make([]byte, discovery.NonceSize)
	request, err := discovery.BuildPreflightRequest(nonce)
	if err != nil {
		t.Fatalf("build discovery request: %v", err)
	}
	if _, err := client.Write(request); err != nil {
		t.Fatalf("write discovery request: %v", err)
	}
	tcpClient, ok := client.(*net.TCPConn)
	if !ok {
		t.Fatalf("client connection type = %T, want *net.TCPConn", client)
	}
	if err := tcpClient.CloseWrite(); err != nil {
		t.Fatalf("close client write side: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set client read deadline: %v", err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read discovery response: %v", err)
	}
	if _, _, err := discovery.ParsePreflightResponse(response, nonce); err != nil {
		t.Fatalf("parse discovery response: %v", err)
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("serveSocket did not return after discovery preflight")
	}
	for _, record := range sink.records {
		if record.message == "transport connection closed" {
			t.Fatalf("discovery preflight emitted an MTProto transport-close record: %+v", record)
		}
	}
}

func assertSingleTransportCloseRecord(t *testing.T, records []serverErrorLogRecord, transport, step, reason string) {
	t.Helper()
	var closeRecords []serverErrorLogRecord
	for _, record := range records {
		if record.message == "transport connection closed" {
			closeRecords = append(closeRecords, record)
		}
	}
	if len(closeRecords) != 1 {
		t.Fatalf("transport close records = %d, want one; all records: %+v", len(closeRecords), records)
	}
	assertTransportCloseRecord(t, closeRecords[0], transport, step, reason)
}

func assertTransportCloseRecord(t *testing.T, record serverErrorLogRecord, transport, step, reason string) {
	t.Helper()
	if record.attrs["transport"] != transport || record.attrs["step"] != step || record.attrs["reason"] != reason {
		t.Fatalf("transport close record = %+v, want %s/%s/%s", record, transport, step, reason)
	}
	for key := range record.attrs {
		if key != "transport" && key != "step" && key != "reason" && key != "suppressed" {
			t.Errorf("transport close record contains unexpected attribute %q: %+v", key, record)
		}
	}
}

type transportCloseEventSink struct {
	events chan serverErrorLogRecord
}

func (*transportCloseEventSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *transportCloseEventSink) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	s.events <- serverErrorLogRecord{message: record.Message, level: record.Level, attrs: attrs}
	return nil
}

func (s *transportCloseEventSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *transportCloseEventSink) WithGroup(string) slog.Handler      { return s }
