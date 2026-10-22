package mtproto

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/exchange"

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
		if key != "transport" && key != "step" && key != "reason" {
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
