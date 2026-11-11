package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/botapi"
)

func TestPublicBotAPIListenerServesClientsPastSharedForwarderBurst(t *testing.T) {
	underlying := listenBotAPITestTCP(t)
	listener := newPublicBotAPIListener(underlying, 1024)

	const clientsCount = 33
	entered := make(chan string, clientsCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	trustedProxy, err := netip.ParsePrefix("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	handler := botapi.WithPublicPath(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Header.Get("Cf-Connecting-Ip")
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), []netip.Prefix{trustedProxy})
	server := botapi.NewServerWithHandler("", handler, slog.New(slog.DiscardHandler))
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close server: %v", err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})

	connections := make([]net.Conn, 0, clientsCount)
	for i := range clientsCount {
		conn, err := dialBotAPITestTCP(t, listener.Addr().String())
		if err != nil {
			t.Fatalf("dial public client %d: %v", i+1, err)
		}
		connections = append(connections, conn)
		clientIP := fmt.Sprintf("198.51.100.%d", i+1)
		request := fmt.Sprintf("GET / HTTP/1.1\r\nHost: example.test\r\nCF-Connecting-IP: %s\r\nConnection: close\r\n\r\n", clientIP)
		if _, err := io.WriteString(conn, request); err != nil {
			t.Fatalf("write public request %d: %v", i+1, err)
		}
	}
	t.Cleanup(func() {
		for _, conn := range connections {
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close client: %v", err)
			}
		}
	})

	seen := make(map[string]bool, clientsCount)
	for range clientsCount {
		select {
		case ip := <-entered:
			seen[ip] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d public requests reached the handler; want %d through one forwarder", len(seen), clientsCount)
		}
	}
	if len(seen) != clientsCount {
		t.Fatalf("distinct public client headers = %d, want %d", len(seen), clientsCount)
	}
	releaseOnce.Do(func() { close(release) })
	for i, conn := range connections {
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatalf("read response %d: %v", i+1, err)
		}
		if response.StatusCode != http.StatusNoContent {
			t.Errorf("response %d status = %d, want 204", i+1, response.StatusCode)
		}
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}
}

func TestTailnetBotAPIListenerRequiresProxyV1(t *testing.T) {
	underlying := listenBotAPITestTCP(t)
	listener := newTailnetBotAPIListener(underlying, slog.New(slog.DiscardHandler))
	server := botapi.NewServerWithHandler("", botapi.WithTailnetPath(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})), slog.New(slog.DiscardHandler))
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close server: %v", err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})

	withoutProxy, err := dialBotAPITestTCP(t, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(withoutProxy, "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := withoutProxy.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(withoutProxy).ReadByte(); err == nil {
		t.Fatal("tailnet listener served HTTP without the required PROXY v1 preface")
	} else if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
		t.Fatalf("tailnet listener did not close a connection without a PROXY preface: %v", err)
	}
	if err := withoutProxy.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close connection without PROXY: %v", err)
	}

	withProxy, err := dialBotAPITestTCP(t, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := withProxy.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close connection with PROXY: %v", err)
		}
	})
	request := "PROXY TCP4 100.64.1.2 100.64.1.3 2345 80\r\nGET / HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(withProxy, request); err != nil {
		t.Fatal(err)
	}
	if err := withProxy.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(withProxy), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("tailnet response status = %d, want 204", response.StatusCode)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeUntilBotAPIErrorCancelsMTProtoServer(t *testing.T) {
	apiErr := errors.New("public Bot API listener failed")
	apiErrors := make(chan error, 1)
	apiErrors <- apiErr
	cancelled := make(chan struct{})
	got := serveUntilBotAPIError(context.Background(), apiErrors, func(ctx context.Context) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	if !errors.Is(got, apiErr) {
		t.Fatalf("serveUntilBotAPIError() = %v, want %v", got, apiErr)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("MTProto server context was not cancelled")
	}
}

func TestServeUntilBotAPIErrorReturnsMTProtoError(t *testing.T) {
	want := errors.New("MTProto server failed")
	got := serveUntilBotAPIError(context.Background(), make(chan error), func(context.Context) error { return want })
	if !errors.Is(got, want) {
		t.Fatalf("serveUntilBotAPIError() = %v, want %v", got, want)
	}
}

func TestBotAPIServeFailureStopsMainServe(t *testing.T) {
	want := errors.New("listener accept failed")
	failures := make(chan error, 1)
	server := botapi.NewServerWithHandler("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), slog.New(slog.DiscardHandler))
	serveBotAPIServer(server, failingBotAPIListener{err: want}, "tailnet", slog.New(slog.DiscardHandler), failures)
	got := serveUntilBotAPIError(context.Background(), failures, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(got, want) {
		t.Fatalf("server error = %v, want wrapped %v", got, want)
	}
}

func TestLoopbackBotAPIServeFailureDoesNotBlockWithoutFailureChannel(t *testing.T) {
	server := botapi.NewServerWithHandler("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), slog.New(slog.DiscardHandler))
	done := serveBotAPIServer(server, failingBotAPIListener{err: errors.New("loopback listener failed")}, "loopback", slog.New(slog.DiscardHandler), nil)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loopback Bot API Serve goroutine blocked while reporting its failure")
	}
}

func listenBotAPITestTCP(t *testing.T) net.Listener {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func dialBotAPITestTCP(t *testing.T, addr string) (net.Conn, error) {
	t.Helper()
	return (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp4", addr)
}

type failingBotAPIListener struct{ err error }

func (l failingBotAPIListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failingBotAPIListener) Close() error              { return nil }
func (l failingBotAPIListener) Addr() net.Addr            { return botAPITestAddr("test") }

type botAPITestAddr string

func (a botAPITestAddr) Network() string { return "test" }
func (a botAPITestAddr) String() string  { return string(a) }
