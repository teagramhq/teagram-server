package mtproto

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/discovery"
	"github.com/teagramhq/teagram-server/internal/store"
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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

	var opened, closed *serverErrorLogRecord
	for i := range sink.records {
		record := &sink.records[i]
		switch record.message {
		case "transport connection opened":
			opened = record
		case "transport connection closed":
			closed = record
		}
	}
	if opened == nil {
		t.Fatalf("no transport open record in %+v", sink.records)
	}
	if closed == nil {
		t.Fatalf("no transport close record in %+v", sink.records)
	}
	connectionID, ok := opened.attrs["connection_id"].(string)
	if !ok || connectionID == "" {
		t.Fatalf("open record connection_id = %v, want non-empty string", opened.attrs["connection_id"])
	}
	if got := closed.attrs["connection_id"]; got != connectionID {
		t.Fatalf("close record connection_id = %v, want open record id %q", got, connectionID)
	}
	for _, record := range []*serverErrorLogRecord{opened, closed} {
		for _, key := range []string{"peer_addr", "client_addr"} {
			if _, ok := record.attrs[key]; ok {
				t.Errorf("%s record contains raw address attribute %q: %+v", record.message, key, record.attrs)
			}
		}
	}
	assertSingleTransportCloseRecord(t, sink.records, "TCP", "none", "client close")
}

func TestServeLogsOpenAndCloseWithAcceptedConnectionID(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()

	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close client: %v", err)
		}
	})
	if _, err := client.Write([]byte{0x01}); err != nil {
		t.Fatalf("write non-discovery prefix: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}

	var opened, closed *serverErrorLogRecord
	deadline := time.After(3 * time.Second)
	for opened == nil || closed == nil {
		select {
		case record := <-sink.events:
			switch record.message {
			case "transport connection opened":
				opened = &record
			case "transport connection closed":
				closed = &record
			}
		case <-deadline:
			cancel()
			t.Fatalf("timed out waiting for open and close records; opened=%+v closed=%+v", opened, closed)
		}
	}
	connectionID, ok := opened.attrs["connection_id"].(string)
	if !ok || connectionID == "" {
		t.Fatalf("open record connection_id = %v, want non-empty string", opened.attrs["connection_id"])
	}
	if got := closed.attrs["connection_id"]; got != connectionID {
		t.Errorf("close record connection_id = %v, want accepted connection id %q", got, connectionID)
	}
	openedAt, ok := opened.attrs["opened_at"].(time.Time)
	if !ok || openedAt.IsZero() {
		t.Fatalf("open record opened_at = %v, want a timestamp", opened.attrs["opened_at"])
	}
	closedOpenedAt, ok := closed.attrs["opened_at"].(time.Time)
	if !ok || !openedAt.Equal(closedOpenedAt) {
		t.Errorf("close record opened_at = %v, want open timestamp %s", closed.attrs["opened_at"], openedAt)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop after cancellation")
	}
}

func TestServeSamplesOpenRecordsDuringConnectCloseFlood(t *testing.T) {
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, listener) }()

	const connections = 25
	for i := range connections {
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
		if err != nil {
			t.Fatalf("dial connection %d: %v", i, err)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close connection %d: %v", i, err)
		}
		waitForLogSamplerDrops(t, &server.preAuthCloseLog, int64(i))
	}
	if got := server.preAuthOpenLog.dropped.Load(); got != int64(connections-1) {
		t.Fatalf("suppressed open records = %d, want %d", got, connections-1)
	}

	// Let the sampler emit the accumulated count at the next interval without
	// making the regression test wait for the production sampling window.
	server.preAuthOpenLog.last.Store(time.Now().Add(-preAuthLogInterval).UnixNano())
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial connection after sample interval: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close connection after sample interval: %v", err)
	}
	waitForLogSamplerDrops(t, &server.preAuthCloseLog, connections)

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop after cancellation")
	}

	var opens []serverErrorLogRecord
	for _, record := range sink.snapshot() {
		if record.message == "transport connection opened" {
			opens = append(opens, record)
		}
	}
	if len(opens) != 2 {
		t.Fatalf("connect-close flood wrote %d open records, want one per sample window: %+v", len(opens), opens)
	}
	if got := opens[1].attrs["suppressed"]; got != int64(connections-1) {
		t.Errorf("suppressed open records = %v, want %d", got, connections-1)
	}
}

func waitForLogSamplerDrops(t *testing.T, sampler *logSampler, want int64) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		if sampler.last.Load() != 0 && sampler.dropped.Load() >= want {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("log sampler dropped %d records, want at least %d", sampler.dropped.Load(), want)
		}
	}
}

func TestServeSocketLogsAuthenticatedIdleTimeoutContext(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(t.Context(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	server := New(exchange.PrivateKey{}, 2, keys, nil, slog.New(sink))
	server.authenticatedReadTimeout = 250 * time.Millisecond
	t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
	if !ok {
		t.Fatal("pre-auth slot unavailable")
	}
	served := make(chan struct{})
	go func() {
		server.serveSocket(serverSocket, slot)
		close(served)
	}()

	clientConn, err := transport.Abridged.Handshake(client)
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	frame := idleReadPingFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})
	if err := clientConn.Send(t.Context(), &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send authenticated ping: %v", err)
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("serveSocket did not return after the authenticated idle timeout")
	}

	var records []serverErrorLogRecord
drain:
	for {
		select {
		case record := <-sink.events:
			records = append(records, record)
		default:
			break drain
		}
	}
	var opened, failure, closed *serverErrorLogRecord
	for i := range records {
		record := &records[i]
		switch record.message {
		case "transport connection opened":
			opened = record
		case "server operation failed":
			if record.attrs["category"] == "idle_timeout" {
				failure = record
			}
		case "transport connection closed":
			closed = record
		}
	}
	if opened == nil || failure == nil || closed == nil {
		t.Fatalf("expected open, idle-timeout, and close records; got %+v", records)
	}
	connectionID, ok := opened.attrs["connection_id"].(string)
	if !ok || connectionID == "" {
		t.Fatalf("open record connection_id = %v, want non-empty string", opened.attrs["connection_id"])
	}
	openedAt, ok := opened.attrs["opened_at"].(time.Time)
	if !ok || openedAt.IsZero() {
		t.Fatalf("open record opened_at = %v, want a timestamp", opened.attrs["opened_at"])
	}
	for _, record := range []*serverErrorLogRecord{failure, closed} {
		if got := record.attrs["connection_id"]; got != connectionID {
			t.Errorf("%s connection_id = %v, want open record id %q", record.message, got, connectionID)
		}
		recordOpenedAt, ok := record.attrs["opened_at"].(time.Time)
		if !ok || !openedAt.Equal(recordOpenedAt) {
			t.Errorf("%s opened_at = %v, want open timestamp %v", record.message, record.attrs["opened_at"], openedAt)
		}
		for _, key := range []string{"peer_addr", "client_addr"} {
			if _, ok := record.attrs[key]; ok {
				t.Errorf("%s record contains raw address attribute %q: %+v", record.message, key, record.attrs)
			}
		}
	}
	for key, want := range map[string]any{
		"category":        "idle_timeout",
		"auth_key_id":     hex.EncodeToString(key.ID[:]),
		"session_id":      int64(42),
		"deadline_source": "default_timeout",
	} {
		if got := failure.attrs[key]; got != want {
			t.Errorf("idle-timeout field %s = %v, want %v", key, got, want)
		}
	}
	lastFrame, ok := failure.attrs["last_frame_received_at"].(time.Time)
	if !ok || lastFrame.IsZero() {
		t.Fatalf("last_frame_received_at = %v, want a timestamp", failure.attrs["last_frame_received_at"])
	}
	readDeadline, ok := failure.attrs["read_deadline"].(time.Time)
	if !ok || !readDeadline.After(lastFrame) {
		t.Fatalf("read_deadline = %v, want after last frame %s", failure.attrs["read_deadline"], lastFrame)
	}
	if delay := readDeadline.Sub(lastFrame); delay < 200*time.Millisecond || delay > 300*time.Millisecond {
		t.Errorf("read deadline is %s after the last frame, want about 250 ms", delay)
	}
	closedAt, ok := failure.attrs["closed_at"].(time.Time)
	if !ok || closedAt.Before(readDeadline) {
		t.Fatalf("closed_at = %v, want at or after read deadline %s", failure.attrs["closed_at"], readDeadline)
	}
	assertTransportCloseRecord(t, *closed, "TCP", "none", "timeout")
}

func TestServeSocketLogsLatestUnknownAuthKeyFrameBeforeIdleTimeout(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(t.Context(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	server := New(exchange.PrivateKey{}, 2, keys, nil, slog.New(sink))
	server.authenticatedReadTimeout = 250 * time.Millisecond
	t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
	if !ok {
		t.Fatal("pre-auth slot unavailable")
	}
	served := make(chan struct{})
	go func() {
		server.serveSocket(serverSocket, slot)
		close(served)
	}()

	clientConn, err := transport.Abridged.Handshake(client)
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	frame := idleReadPingFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})
	if err := clientConn.Send(t.Context(), &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send authenticated ping: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	var unknownRaw crypto.Key
	for i := range unknownRaw {
		unknownRaw[i] = byte(i + 91)
	}
	unknownKey := unknownRaw.WithID()
	unknownSentAt := time.Now()
	unknownFrame := idleReadPingFrame(t, unknownKey, 42, 2<<32, &mt.PingRequest{PingID: 2})
	if err := clientConn.Send(t.Context(), &bin.Buffer{Buf: unknownFrame}); err != nil {
		t.Fatalf("send unknown-auth-key frame: %v", err)
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("serveSocket did not return after the idle timeout")
	}

	var failure *serverErrorLogRecord
drain:
	for {
		select {
		case record := <-sink.events:
			if record.message == "server operation failed" && record.attrs["category"] == "idle_timeout" {
				failure = &record
			}
		default:
			break drain
		}
	}
	if failure == nil {
		t.Fatalf("no idle-timeout record in logs")
	}
	lastFrame, ok := failure.attrs["last_frame_received_at"].(time.Time)
	if !ok || lastFrame.Before(unknownSentAt.Add(-10*time.Millisecond)) {
		t.Errorf("last_frame_received_at = %v, want the later unknown-key read near %s", failure.attrs["last_frame_received_at"], unknownSentAt)
	}
	if failure.attrs["auth_key_id"] != hex.EncodeToString(key.ID[:]) {
		t.Errorf("idle-timeout auth_key_id = %v, want authenticated key %s", failure.attrs["auth_key_id"], hex.EncodeToString(key.ID[:]))
	}
}

func TestServeSocketLogsPingDelayDisconnectDeadlineSource(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(t.Context(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	server := New(exchange.PrivateKey{}, 2, keys, nil, slog.New(sink))
	server.authenticatedReadTimeout = 250 * time.Millisecond
	t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
	if !ok {
		t.Fatal("pre-auth slot unavailable")
	}
	served := make(chan struct{})
	go func() {
		server.serveSocket(serverSocket, slot)
		close(served)
	}()
	clientConn, err := transport.Abridged.Handshake(client)
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	frame := idleReadPingFrame(t, key, 42, 1<<32, &mt.PingDelayDisconnectRequest{PingID: 1, DisconnectDelay: 1})
	if err := clientConn.Send(t.Context(), &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send ping_delay_disconnect: %v", err)
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("serveSocket did not return after ping_delay_disconnect deadline")
	}

	var failure *serverErrorLogRecord
drain:
	for {
		select {
		case record := <-sink.events:
			if record.message == "server operation failed" && record.attrs["category"] == "idle_timeout" {
				failure = &record
			}
		default:
			break drain
		}
	}
	if failure == nil {
		t.Fatalf("no idle-timeout record in logs")
	}
	if got := failure.attrs["deadline_source"]; got != "client_ping_delay_disconnect" {
		t.Errorf("deadline_source = %v, want client_ping_delay_disconnect", got)
	}
	readDeadline, ok := failure.attrs["read_deadline"].(time.Time)
	if !ok || readDeadline.IsZero() {
		t.Errorf("read_deadline = %v, want a timestamp", failure.attrs["read_deadline"])
	}
	lastFrame, ok := failure.attrs["last_frame_received_at"].(time.Time)
	if !ok || readDeadline.Sub(lastFrame) < 900*time.Millisecond || readDeadline.Sub(lastFrame) > 1100*time.Millisecond {
		t.Errorf("read_deadline = %v, want about one second after last_frame_received_at %v", readDeadline, lastFrame)
	}
}

func TestReadReqDHTimeoutLogsStageTiming(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	sink := &serverErrorLogSink{}
	server := New(exchange.PrivateKey{RSA: rsaKey}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	connectionID := server.newConnectionID()
	openedAt := time.Now()
	diagnostics := newTransportCloseDiagnostics(transportTCP, connectionID, openedAt)
	server.logConnectionOpened(transportTCP, connectionID, openedAt)
	conn := &serverLoopFailureConn{err: context.DeadlineExceeded}
	first := &bin.Buffer{Buf: exchangeRequestFrame(t)}
	var attempts int
	err = server.runExchangeWithCloseDiagnostics(
		context.Background(), conn, first, netip.Addr{}, diagnostics, &attempts, store.RateLimitListenerTailnet,
	)
	if err == nil {
		t.Fatal("key exchange succeeded after req_DH_params read timed out")
	}
	server.logConnectionFailure(err, diagnostics)
	server.logTransportClose(diagnostics, err)

	var opened, failure, closed *serverErrorLogRecord
	for i := range sink.records {
		record := &sink.records[i]
		switch record.message {
		case "transport connection opened":
			opened = record
		case "server operation failed":
			failure = record
		case "transport connection closed":
			closed = record
		}
	}
	if opened == nil || failure == nil || closed == nil {
		t.Fatalf("expected open, exchange failure, and transport close records, got %+v", sink.records)
	}
	if opened.attrs["connection_id"] != connectionID {
		t.Fatalf("open record connection_id = %v, want %q", opened.attrs["connection_id"], connectionID)
	}
	for _, record := range []*serverErrorLogRecord{failure, closed} {
		if record.attrs["connection_id"] != connectionID {
			t.Errorf("%s connection_id = %v, want %q", record.message, record.attrs["connection_id"], connectionID)
		}
		recordOpenedAt, ok := record.attrs["opened_at"].(time.Time)
		if !ok || !openedAt.Equal(recordOpenedAt) {
			t.Errorf("%s opened_at = %v, want %s", record.message, record.attrs["opened_at"], openedAt)
		}
	}
	for key, want := range map[string]any{
		"category": "exchange",
		"stage":    "read_req_dh",
		"cause":    "timeout",
	} {
		if got := failure.attrs[key]; got != want {
			t.Errorf("exchange field %s = %v, want %v", key, got, want)
		}
	}
	stageStartedAt, ok := failure.attrs["stage_started_at"].(time.Time)
	if !ok || stageStartedAt.IsZero() {
		t.Fatalf("stage_started_at = %v, want a timestamp", failure.attrs["stage_started_at"])
	}
	gaveUpAt, ok := failure.attrs["gave_up_at"].(time.Time)
	if !ok || gaveUpAt.Before(stageStartedAt) {
		t.Fatalf("gave_up_at = %v, want at or after stage start %s", failure.attrs["gave_up_at"], stageStartedAt)
	}
	if closed.attrs["stage"] != "read_req_dh" || closed.attrs["stage_started_at"] != stageStartedAt || closed.attrs["gave_up_at"] != gaveUpAt {
		t.Errorf("transport close exchange timing = %+v, want read_req_dh with matching stage timestamps", closed.attrs)
	}
	for _, record := range []*serverErrorLogRecord{failure, closed} {
		for _, key := range []string{"peer_addr", "client_addr", "auth_key", "payload"} {
			if _, ok := record.attrs[key]; ok {
				t.Errorf("%s record contains forbidden attribute %q: %+v", record.message, key, record.attrs)
			}
		}
	}
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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
		slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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

	heldSlot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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
	server.preAuth.limits.MaxConns = 1
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
	failedWS, response, err := websocket.Dial(dialCtx, "ws://"+listener.Addr().String()+"/not-apiws", &websocket.DialOptions{
		Subprotocols: []string{"binary"},
	})
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close failed-upgrade response body: %v", closeErr)
		}
	}
	if err == nil {
		if closeErr := failedWS.CloseNow(); closeErr != nil && !isDisconnect(closeErr) {
			t.Errorf("close unexpected WebSocket on failed upgrade: %v", closeErr)
		}
		t.Fatal("WebSocket upgrade on an invalid path succeeded")
	}
drainFailedUpgradeLogs:
	for {
		select {
		case record := <-sink.events:
			if record.message == "transport connection opened" {
				t.Fatalf("failed upgrade emitted an open record before admission: %+v", record)
			}
		default:
			break drainFailedUpgradeLogs
		}
	}

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
	refusedWS, response, err := websocket.Dial(dialCtx, "ws://"+listener.Addr().String()+"/apiws", &websocket.DialOptions{
		Subprotocols: []string{"binary"},
	})
	if response != nil && response.Body != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Errorf("close refused-upgrade response body: %v", closeErr)
		}
	}
	if err == nil {
		if closeErr := refusedWS.CloseNow(); closeErr != nil && !isDisconnect(closeErr) {
			t.Errorf("close unexpected WebSocket past pre-auth cap: %v", closeErr)
		}
		t.Fatal("WebSocket upgrade was admitted past the pre-auth cap")
	}
	if err := ws.CloseNow(); err != nil {
		t.Fatalf("close WebSocket before codec detection: %v", err)
	}

	var openRecord, closeRecord *serverErrorLogRecord
	openRecords := 0
	refusedLogged := false
	deadline := time.After(3 * time.Second)
	for openRecord == nil || closeRecord == nil || !refusedLogged {
		select {
		case record := <-sink.events:
			switch record.message {
			case "transport connection opened":
				openRecords++
				if openRecord == nil {
					openRecord = &record
				}
			case "transport connection closed":
				closeRecord = &record
			case "WebSocket connection refused at the pre-auth cap":
				refusedLogged = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for WebSocket open, refusal, and close records; open=%+v close=%+v refused=%t", openRecord, closeRecord, refusedLogged)
		}
	}
	if openRecords != 1 {
		t.Errorf("WebSocket open records = %d, want one for the accepted upgrade", openRecords)
	}
	if got := server.preAuthOpenLog.dropped.Load(); got != 0 {
		t.Errorf("suppressed WebSocket open records = %d, want none for failed or refused upgrades", got)
	}
	connectionID, ok := openRecord.attrs["connection_id"].(string)
	if !ok || connectionID == "" {
		t.Fatalf("open record connection_id = %v, want non-empty string", openRecord.attrs["connection_id"])
	}
	if openRecord.attrs["transport"] != websocketPath {
		t.Errorf("open record transport = %v, want %q", openRecord.attrs["transport"], websocketPath)
	}
	openedAt, ok := openRecord.attrs["opened_at"].(time.Time)
	if !ok || openedAt.IsZero() {
		t.Fatalf("open record opened_at = %v, want a timestamp", openRecord.attrs["opened_at"])
	}
	if got := closeRecord.attrs["connection_id"]; got != connectionID {
		t.Errorf("close record connection_id = %v, want open record id %q", got, connectionID)
	}
	closedOpenedAt, ok := closeRecord.attrs["opened_at"].(time.Time)
	if !ok || !openedAt.Equal(closedOpenedAt) {
		t.Errorf("close record opened_at = %v, want open timestamp %s", closeRecord.attrs["opened_at"], openedAt)
	}
	for _, record := range []*serverErrorLogRecord{openRecord, closeRecord} {
		for _, key := range []string{"peer_addr", "client_addr"} {
			if _, ok := record.attrs[key]; ok {
				t.Errorf("%s record contains raw address attribute %q: %+v", record.message, key, record.attrs)
			}
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

func TestServeWebSocketLogsAuthenticatedIdleTimeoutContext(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	key := idleReadTestKey()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(t.Context(), key); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	server := New(exchange.PrivateKey{}, 2, keys, nil, slog.New(sink))
	server.authenticatedReadTimeout = 250 * time.Millisecond
	t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
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
	client, err := transport.Intermediate.Handshake(websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	frame := idleReadPingFrame(t, key, 42, 1<<32, &mt.PingRequest{PingID: 1})
	if err := client.Send(dialCtx, &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send authenticated ping: %v", err)
	}

	var opened, failure, closed *serverErrorLogRecord
	deadline := time.After(3 * time.Second)
	for opened == nil || failure == nil || closed == nil {
		select {
		case record := <-sink.events:
			switch record.message {
			case "transport connection opened":
				opened = &record
			case "server operation failed":
				if record.attrs["category"] == "idle_timeout" {
					failure = &record
				}
			case "transport connection closed":
				closed = &record
			}
		case <-deadline:
			t.Fatalf("timed out waiting for WebSocket open, idle-timeout, and close records; open=%+v failure=%+v close=%+v", opened, failure, closed)
		}
	}
	connectionID, ok := opened.attrs["connection_id"].(string)
	if !ok || connectionID == "" {
		t.Fatalf("open record connection_id = %v, want non-empty string", opened.attrs["connection_id"])
	}
	openedAt, ok := opened.attrs["opened_at"].(time.Time)
	if !ok || openedAt.IsZero() {
		t.Fatalf("open record opened_at = %v, want a timestamp", opened.attrs["opened_at"])
	}
	for _, record := range []*serverErrorLogRecord{failure, closed} {
		if got := record.attrs["connection_id"]; got != connectionID {
			t.Errorf("%s connection_id = %v, want open record id %q", record.message, got, connectionID)
		}
		recordOpenedAt, ok := record.attrs["opened_at"].(time.Time)
		if !ok || !openedAt.Equal(recordOpenedAt) {
			t.Errorf("%s opened_at = %v, want open timestamp %s", record.message, record.attrs["opened_at"], openedAt)
		}
		for _, key := range []string{"peer_addr", "client_addr"} {
			if _, ok := record.attrs[key]; ok {
				t.Errorf("%s record contains raw address attribute %q: %+v", record.message, key, record.attrs)
			}
		}
	}
	for key, want := range map[string]any{
		"category":        "idle_timeout",
		"auth_key_id":     hex.EncodeToString(key.ID[:]),
		"session_id":      int64(42),
		"deadline_source": "default_timeout",
	} {
		if got := failure.attrs[key]; got != want {
			t.Errorf("idle-timeout field %s = %v, want %v", key, got, want)
		}
	}
	lastFrame, ok := failure.attrs["last_frame_received_at"].(time.Time)
	if !ok || lastFrame.IsZero() {
		t.Fatalf("last_frame_received_at = %v, want a timestamp", failure.attrs["last_frame_received_at"])
	}
	readDeadline, ok := failure.attrs["read_deadline"].(time.Time)
	if !ok || !readDeadline.After(lastFrame) {
		t.Fatalf("read_deadline = %v, want after last frame %s", failure.attrs["read_deadline"], lastFrame)
	}
	closedAt, ok := failure.attrs["closed_at"].(time.Time)
	if !ok || closedAt.Before(readDeadline) {
		t.Fatalf("closed_at = %v, want at or after read deadline %s", failure.attrs["closed_at"], readDeadline)
	}
	assertTransportCloseRecord(t, *closed, websocketPath, "none", "timeout")

	cancel()
	select {
	case err := <-served:
		serverReturned = true
		if err != nil {
			t.Fatalf("serve WebSocket: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeWebSocket did not stop after the idle timeout")
	}
}

func TestServeWebSocketLogsCloseAfterUnknownAuthKeyAsClientDisconnect(t *testing.T) {
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

	client, err := transport.Intermediate.Handshake(websocket.NetConn(context.Background(), ws, websocket.MessageBinary))
	if err != nil {
		t.Fatalf("transport handshake: %v", err)
	}
	var frame bin.Buffer
	frame.Put(make([]byte, 16))
	frame.Buf[0] = 1
	if err := client.Send(dialCtx, &frame); err != nil {
		t.Fatalf("send unknown-key frame: %v", err)
	}
	var responseFrame bin.Buffer
	if err := client.Recv(dialCtx, &responseFrame); err == nil {
		t.Fatal("receive unknown-key response succeeded, want -404")
	} else {
		var protocolErr *codec.ProtocolErr
		if !errors.As(err, &protocolErr) || protocolErr.Code != codec.CodeAuthKeyNotFound {
			t.Fatalf("receive unknown-key response = %v, want -404", err)
		}
	}
	if err := ws.Close(websocket.StatusPolicyViolation, "client disconnect"); err != nil {
		t.Fatalf("close WebSocket after -404: %v", err)
	}

	var records []serverErrorLogRecord
	waitCtx, stopWait := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopWait()
	for {
		select {
		case record := <-sink.events:
			records = append(records, record)
			if record.message != "transport connection closed" {
				continue
			}
			cancel()
			select {
			case err := <-served:
				serverReturned = true
				if err != nil {
					t.Fatalf("serve WebSocket: %v", err)
				}
			case <-waitCtx.Done():
				t.Fatal("ServeWebSocket did not stop after the client closed")
			}
			for {
				select {
				case record := <-sink.events:
					records = append(records, record)
				default:
					var closeRecords []serverErrorLogRecord
					for _, record := range records {
						if record.message == "transport connection closed" {
							closeRecords = append(closeRecords, record)
						}
					}
					if len(closeRecords) != 1 {
						t.Fatalf("transport close records = %d, want one; all records: %+v", len(closeRecords), records)
					}
					for key, want := range map[string]any{
						"transport": "/apiws",
						"step":      "none",
						"reason":    "client close",
					} {
						if got := closeRecords[0].attrs[key]; got != want {
							t.Errorf("transport close %s = %v, want %v", key, got, want)
						}
					}
					for _, record := range records {
						if record.message == "server operation failed" {
							t.Fatalf("client close after -404 logged a server failure: %+v", record)
						}
					}
					return
				}
			}
		case <-waitCtx.Done():
			t.Fatal("no transport close log after client closed following -404")
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
			var openRecord, ceilingRecord *serverErrorLogRecord
			for i := range records {
				record := &records[i]
				switch record.message {
				case "transport connection opened":
					openRecord = record
				case "WebSocket connection closed at the pre-auth lifetime ceiling":
					ceilingRecord = record
				}
			}
			if openRecord == nil || ceilingRecord == nil {
				t.Fatalf("expected WebSocket open and pre-auth lifetime records; got %+v", records)
			}
			connectionID, ok := openRecord.attrs["connection_id"].(string)
			if !ok || connectionID == "" {
				t.Fatalf("open record connection_id = %v, want a non-empty ID", openRecord.attrs["connection_id"])
			}
			if got := ceilingRecord.attrs["connection_id"]; got != connectionID {
				t.Errorf("pre-auth lifetime record connection_id = %v, want open record ID %q", got, connectionID)
			}
			openedAt, ok := openRecord.attrs["opened_at"].(time.Time)
			if !ok || openedAt.IsZero() {
				t.Fatalf("open record opened_at = %v, want a timestamp", openRecord.attrs["opened_at"])
			}
			lifetimeOpenedAt, ok := ceilingRecord.attrs["opened_at"].(time.Time)
			if !ok || !openedAt.Equal(lifetimeOpenedAt) {
				t.Errorf("pre-auth lifetime record opened_at = %v, want open timestamp %s", ceilingRecord.attrs["opened_at"], openedAt)
			}
			return
		}
	}
}

type delayedWebSocketResponseListener struct {
	net.Listener

	writeStarted chan struct{}
	releaseWrite chan struct{}
	releaseOnce  sync.Once
}

func (l *delayedWebSocketResponseListener) release() {
	l.releaseOnce.Do(func() { close(l.releaseWrite) })
}

func (l *delayedWebSocketResponseListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &delayedWebSocketResponseConn{
		Conn:         conn,
		writeStarted: l.writeStarted,
		releaseWrite: l.releaseWrite,
	}, nil
}

type delayedWebSocketResponseConn struct {
	net.Conn

	writeStarted chan struct{}
	releaseWrite <-chan struct{}
	writeOnce    sync.Once
}

func (c *delayedWebSocketResponseConn) Write(p []byte) (int, error) {
	c.writeOnce.Do(func() {
		close(c.writeStarted)
		<-c.releaseWrite
	})
	return c.Conn.Write(p)
}

func TestServeWebSocketLifetimeExpiryRacesSuccessfulUpgrade(t *testing.T) {
	sink := &transportCloseEventSink{events: make(chan serverErrorLogRecord, 16)}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, slog.New(sink))
	const lifetime = 2 * time.Second
	if err := server.SetPreAuthLimits(PreAuthLimits{Lifetime: lifetime}); err != nil {
		t.Fatalf("set pre-auth lifetime: %v", err)
	}
	baseListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := &delayedWebSocketResponseListener{
		Listener:     baseListener,
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(ctx, listener) }()
	serverReturned := false
	t.Cleanup(func() {
		listener.release()
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

	type dialResult struct {
		conn *websocket.Conn
		err  error
	}
	dialCtx, stopDial := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopDial()
	dialed := make(chan dialResult, 1)
	go func() {
		conn, response, err := websocket.Dial(dialCtx, "ws://"+baseListener.Addr().String()+"/apiws", &websocket.DialOptions{
			Subprotocols: []string{"binary"},
		})
		if response != nil && response.Body != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Errorf("close WebSocket response body: %v", closeErr)
			}
		}
		dialed <- dialResult{conn: conn, err: err}
	}()
	select {
	case <-listener.writeStarted:
	case <-dialCtx.Done():
		t.Fatal("WebSocket upgrade did not reach its response write")
	}
	select {
	case <-time.After(lifetime + 100*time.Millisecond):
	case <-dialCtx.Done():
		t.Fatal("WebSocket upgrade did not remain blocked for the pre-auth lifetime")
	}
	listener.release()
	var dialedConn *websocket.Conn
	select {
	case result := <-dialed:
		if result.err != nil {
			t.Fatalf("WebSocket upgrade failed after releasing response write: %v", result.err)
		}
		dialedConn = result.conn
	case <-dialCtx.Done():
		t.Fatal("WebSocket upgrade did not complete after releasing response write")
	}
	t.Cleanup(func() {
		if err := dialedConn.CloseNow(); err != nil && !isDisconnect(err) {
			t.Errorf("close WebSocket client: %v", err)
		}
	})

	var records []serverErrorLogRecord
	for {
		select {
		case record := <-sink.events:
			records = append(records, record)
			if record.message != "WebSocket connection closed at the pre-auth lifetime ceiling" {
				continue
			}
			var openRecord, ceilingRecord *serverErrorLogRecord
			openIndex, closeIndex := -1, -1
			for i := range records {
				record := &records[i]
				switch record.message {
				case "transport connection opened":
					openRecord = record
					openIndex = i
				case "WebSocket connection closed at the pre-auth lifetime ceiling":
					ceilingRecord = record
					closeIndex = i
				}
			}
			if openRecord == nil || ceilingRecord == nil {
				t.Fatalf("expected open and pre-auth lifetime records; got %+v", records)
			}
			connectionID, ok := openRecord.attrs["connection_id"].(string)
			if !ok || connectionID == "" {
				t.Fatalf("open record connection_id = %v, want a non-empty ID", openRecord.attrs["connection_id"])
			}
			if got := ceilingRecord.attrs["connection_id"]; got != connectionID {
				t.Errorf("pre-auth lifetime record connection_id = %v, want open record ID %q", got, connectionID)
			}
			openedAt, ok := openRecord.attrs["opened_at"].(time.Time)
			if !ok || openedAt.IsZero() {
				t.Fatalf("open record opened_at = %v, want a timestamp", openRecord.attrs["opened_at"])
			}
			lifetimeOpenedAt, ok := ceilingRecord.attrs["opened_at"].(time.Time)
			if !ok || !openedAt.Equal(lifetimeOpenedAt) {
				t.Errorf("pre-auth lifetime record opened_at = %v, want open timestamp %s", ceilingRecord.attrs["opened_at"], openedAt)
			}
			if openIndex < 0 || closeIndex < 0 || openIndex >= closeIndex {
				t.Errorf("pre-auth lifetime record preceded the open record: %+v", records)
			}
			cancel()
			select {
			case err := <-served:
				serverReturned = true
				if err != nil {
					t.Fatalf("serve WebSocket: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ServeWebSocket did not stop after the pre-auth lifetime close")
			}
			return
		case <-dialCtx.Done():
			t.Fatal("WebSocket pre-auth lifetime did not expire after the upgrade")
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
	slot, ok := server.preAuth.admit(store.RateLimitListenerTailnet)
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
	allowed := map[string]struct{}{
		"transport": {}, "step": {}, "reason": {}, "suppressed": {},
		"connection_id": {}, "opened_at": {}, "closed_at": {},
		"stage": {}, "stage_started_at": {}, "gave_up_at": {},
	}
	if transport == websocketPath {
		allowed["auth_key_id"] = struct{}{}
	}
	for key := range record.attrs {
		if _, ok := allowed[key]; !ok {
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
