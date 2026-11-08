package mtproto

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

type shutdownTestListener struct{}

func (shutdownTestListener) Accept() (net.Conn, error) { return nil, errors.New("unused") }
func (shutdownTestListener) Close() error              { return nil }
func (shutdownTestListener) Addr() net.Addr            { return shutdownTestAddr("unused") }

type shutdownTestAddr string

func (a shutdownTestAddr) Network() string { return string(a) }
func (a shutdownTestAddr) String() string  { return string(a) }

func TestWebSocketWaitTracksHandlerAfterSocketClose(t *testing.T) {
	listener := newWebSocketListener(shutdownTestListener{}, nil, store.RateLimitListenerTailnet, "")
	close(listener.acceptDone)
	serverConn, peerConn := net.Pipe()
	defer func() {
		if err := peerConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close peer socket: %v", err)
		}
	}()
	accepted := &webSocketAcceptedConn{Conn: serverConn, listener: listener}
	if !listener.track(accepted) {
		t.Fatal("failed to track accepted socket")
	}
	if !listener.trackHandler(accepted) {
		t.Fatal("failed to track HTTP handler")
	}
	if err := accepted.Close(); err != nil {
		t.Fatalf("close accepted socket: %v", err)
	}

	waitDone := make(chan bool, 1)
	go func() { waitDone <- listener.wait(context.Background()) }()
	select {
	case <-waitDone:
		t.Fatal("listener wait returned when the socket closed but the hijacked handler was still active")
	case <-time.After(20 * time.Millisecond):
	}

	accepted.handlerDone()
	select {
	case ok := <-waitDone:
		if !ok {
			t.Fatal("listener wait reported failure after handler completion")
		}
	case <-time.After(time.Second):
		t.Fatal("listener wait did not finish after handler completion")
	}
}

func TestHandleWebSocketDoesNotKeyAddressAfterAcceptedSocketCloses(t *testing.T) {
	limiter := newPreAuthLimiter(PreAuthLimits{MaxConnsPerNet: 1})
	slot, ok := limiter.admit(store.RateLimitListenerPublic)
	if !ok {
		t.Fatal("failed to admit test socket")
	}
	serverConn, peerConn := net.Pipe()
	defer func() {
		if err := peerConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close peer socket: %v", err)
		}
	}()
	accepted := &webSocketAcceptedConn{Conn: serverConn, slot: slot}
	state := &webSocketConnState{socket: accepted, slot: slot}
	if err := accepted.Close(); err != nil {
		t.Fatalf("close accepted socket: %v", err)
	}

	server := &Server{preAuth: limiter, log: slog.New(slog.DiscardHandler)}
	request := httptest.NewRequestWithContext(
		context.WithValue(context.Background(), webSocketConnKey{}, state),
		http.MethodGet, "/not-apiws", nil,
	)
	request.Header.Set("Cf-Connecting-Ip", "198.51.100.41")
	server.handleWebSocket(httptest.NewRecorder(), request, nil, nil, false, nil, cfConnectingIPHeader)

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.perNet) != 0 {
		t.Fatalf("closed accepted socket left network buckets charged: %v", limiter.perNet)
	}
}

func TestHandleWebSocketLogsInvalidCFConnectingIPThrottled(t *testing.T) {
	var logs bytes.Buffer
	server := &Server{
		preAuth: newPreAuthLimiter(PreAuthLimits{MaxConnsPerNet: 1}),
		log:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	tests := []struct {
		name     string
		header   http.Header
		wantText string
	}{
		{name: "malformed", header: http.Header{"Cf-Connecting-Ip": {"raw-untrusted-value"}}, wantText: "CF-Connecting-IP must be exactly one IP address"},
		{name: "missing", wantText: "CF-Connecting-IP must appear exactly once"},
		{name: "duplicate", header: http.Header{"Cf-Connecting-Ip": {"198.51.100.42", "203.0.113.42"}}, wantText: "CF-Connecting-IP must appear exactly once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverConn, peerConn := net.Pipe()
			defer func() {
				if err := peerConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Errorf("close peer socket: %v", err)
				}
			}()
			defer func() {
				if err := serverConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Errorf("close server socket: %v", err)
				}
			}()
			slot, ok := server.preAuth.admit(store.RateLimitListenerPublic)
			if !ok {
				t.Fatal("failed to admit test socket")
			}
			state := &webSocketConnState{
				socket: &webSocketAcceptedConn{Conn: serverConn, slot: slot},
				slot:   slot,
			}
			request := httptest.NewRequestWithContext(
				context.WithValue(context.Background(), webSocketConnKey{}, state),
				http.MethodGet, "/not-apiws", nil,
			)
			request.Header = tt.header.Clone()
			recorder := httptest.NewRecorder()
			server.handleWebSocket(recorder, request, nil, nil, false, nil, cfConnectingIPHeader)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("response status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if body := strings.TrimSpace(recorder.Body.String()); body != tt.wantText {
				t.Errorf("response body = %q, want %q", body, tt.wantText)
			}
		})
	}

	logText := logs.String()
	if got := strings.Count(logText, "invalid CF-Connecting-IP header"); got != 1 {
		t.Errorf("invalid-header log count = %d, want one throttled line:\n%s", got, logText)
	}
	for _, want := range []string{"header=CF-Connecting-IP", "value_count=1"} {
		if !strings.Contains(logText, want) {
			t.Errorf("throttled log does not contain %q:\n%s", want, logText)
		}
	}
	for _, raw := range []string{"raw-untrusted-value", "198.51.100.42", "203.0.113.42"} {
		if strings.Contains(logText, raw) {
			t.Errorf("logs contain raw header value %q: %s", raw, logText)
		}
	}
}
