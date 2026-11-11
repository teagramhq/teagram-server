package botapi_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/botapi"
	"github.com/teagramhq/teagram-server/internal/store"
)

func proxyServer(t *testing.T, limit int, remote string) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if remote != "" {
		ln = &overrideListener{Listener: ln, remote: remote}
	}
	srv := botapi.NewServer("", newFakeStore(), slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(botapi.LimitConnectionsPerIP(ln, limit)) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})
	return ln.Addr().String()
}

func socketLimitServer(t *testing.T, limit int) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := botapi.NewServer("", newFakeStore(), slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(botapi.LimitConnectionsPerSocketIP(ln, limit)) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})
	return ln.Addr().String()
}

func requiredProxyServer(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Remote-Addr", r.RemoteAddr)
	})
	srv := botapi.NewServerWithHandler("", handler, slog.New(slog.DiscardHandler))
	done := make(chan error, 1)
	go func() { done <- srv.Serve(botapi.RequireProxyV1(ln)) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})
	return ln.Addr().String()
}

type overrideListener struct {
	net.Listener

	remote string
}

func (l *overrideListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &overrideConn{Conn: c, remote: l.remote}, nil
}

type overrideConn struct {
	net.Conn

	remote string
}

func (c *overrideConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(c.remote), Port: 1234}
}

func proxyDial(t *testing.T, addr, header string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, header); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewReader(conn)
}
func proxyRequest(t *testing.T, conn net.Conn, reader *bufio.Reader) int {
	t.Helper()
	if _, err := io.WriteString(conn, "GET /invalid HTTP/1.1\r\nHost: example\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}
func proxyHeader(ip string) string { return fmt.Sprintf("PROXY TCP4 %s 100.64.0.1 1234 2447\r\n", ip) }

func exhaustAuthBucket(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		switch status := proxyRequest(t, conn, reader); status {
		case http.StatusTooManyRequests:
			return
		case http.StatusUnauthorized:
		default:
			t.Fatalf("request %d status = %d, want 401 or 429", i, status)
		}
	}
	t.Fatal("auth bucket did not exhaust within 200 requests")
}

func TestProxyPeersHaveIndependentAuthBuckets(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"ipv4 same subnet", proxyHeader("100.64.1.2"), proxyHeader("100.64.1.3")},
		{"ipv6 same subnet", "PROXY TCP6 fd7a:115c:a1e0::1 fd7a:115c:a1e0::10 1234 2447\r\n", "PROXY TCP6 fd7a:115c:a1e0::2 fd7a:115c:a1e0::10 1234 2447\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := proxyServer(t, 10, "")
			a, ar := proxyDial(t, addr, tc.first)
			exhaustAuthBucket(t, a, ar)
			b, br := proxyDial(t, addr, tc.second)
			if status := proxyRequest(t, b, br); status != http.StatusUnauthorized {
				t.Fatalf("other peer status = %d, want 401", status)
			}
		})
	}
}

func TestProxyMappedAndIPv4AddressesShareAuthBucket(t *testing.T) {
	addr := proxyServer(t, 10, "")
	conn, reader := proxyDial(t, addr, proxyHeader("100.64.1.2"))
	mapped, mappedReader := proxyDial(t, addr, "PROXY TCP6 ::ffff:100.64.1.2 ::ffff:100.64.0.1 5555 2447\r\n")
	if err := mapped.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Fewer than the burst size of probes cannot exhaust a separate mapped bucket.
	for range 10 {
		exhaustAuthBucket(t, conn, reader)
		switch status := proxyRequest(t, mapped, mappedReader); status {
		case http.StatusTooManyRequests:
			return
		case http.StatusUnauthorized:
		default:
			t.Fatalf("mapped peer status = %d, want 401 or 429", status)
		}
	}
	t.Fatal("mapped peer did not share the exhausted IPv4 bucket")
}

func TestProxyPeersHaveIndependentConnectionCaps(t *testing.T) {
	addr := proxyServer(t, 1, "")
	a, ar := proxyDial(t, addr, proxyHeader("100.64.1.2"))
	if status := proxyRequest(t, a, ar); status != http.StatusUnauthorized {
		t.Fatalf("first status = %d", status)
	}
	b, br := proxyDial(t, addr, proxyHeader("100.64.1.3"))
	if status := proxyRequest(t, b, br); status != http.StatusUnauthorized {
		t.Fatalf("other peer status = %d, want 401", status)
	}
	_, er := proxyDial(t, addr, proxyHeader("100.64.1.2")+"GET /invalid HTTP/1.1\r\nHost: example\r\n\r\n")
	if resp, err := http.ReadResponse(er, nil); err == nil {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("excess connection was accepted")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("excess connection not closed: %v", err)
	}
}

func TestProxyHeaderFromNonLoopbackRejected(t *testing.T) {
	addr := proxyServer(t, 10, "192.0.2.5")
	_, reader := proxyDial(t, addr, proxyHeader("100.64.1.2")+"GET /invalid HTTP/1.1\r\nHost: example\r\n\r\n")
	if response, err := http.ReadResponse(reader, nil); err == nil {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
		t.Fatalf("forged header returned HTTP %d", response.StatusCode)
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("forged connection not closed: %v", err)
	}
}

func TestSocketLimitListenerDoesNotAcceptProxyV1Preface(t *testing.T) {
	addr := socketLimitServer(t, 10)
	_, reader := proxyDial(t, addr, proxyHeader("203.0.113.7")+"GET /invalid HTTP/1.1\r\nHost: example\r\n\r\n")
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read HTTP response after PROXY preface: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("PROXY preface returned HTTP %d, want HTTP 400 from the HTTP parser", response.StatusCode)
	}
}

func TestRequiredProxyV1ListenerDecodesTailnetPeer(t *testing.T) {
	addr := requiredProxyServer(t)
	conn, reader := proxyDial(t, addr, proxyHeader("100.64.1.2"))
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read HTTP response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := response.Header.Get("X-Remote-Addr"), "100.64.1.2:1234"; got != want {
		t.Fatalf("remote address = %q, want %q", got, want)
	}
}

func TestRequiredProxyV1ListenerRejectsMissingPreface(t *testing.T) {
	addr := requiredProxyServer(t)
	conn, reader := proxyDial(t, addr, "")
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if response, err := http.ReadResponse(reader, nil); err == nil {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
		t.Fatalf("HTTP request without a PROXY preface returned status %d", response.StatusCode)
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("connection without a PROXY preface was not closed: %v", err)
	}
}

func TestLoopbackWithoutProxyHeaderStillWorks(t *testing.T) {
	addr := proxyServer(t, 10, "")
	conn, reader := proxyDial(t, addr, "")
	if status := proxyRequest(t, conn, reader); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestMalformedProxyHeaderRejected(t *testing.T) {
	addr := proxyServer(t, 10, "")
	for _, header := range []string{"PROXY TCP4 999.0.0.1 100.64.0.1 1234 2447\r\n", "PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\n", "PROXY " + strings.Repeat("x", 110) + "\r\n", "PROXY TCP4 100.64.1.2 100.64.0.1 +12 2447\r\n", "PROXY TCP6 100.64.1.2 100.64.0.1 12 2447\r\n"} {
		_, reader := proxyDial(t, addr, header+"GET /invalid HTTP/1.1\r\nHost: example\r\n\r\n")
		if resp, err := http.ReadResponse(reader, nil); err == nil {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
			t.Fatalf("malformed header %q returned HTTP %d", header, resp.StatusCode)
		} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
			t.Fatalf("malformed header not closed: %v", err)
		}
	}
}

func TestIncompletePrefaceDoesNotBlockOtherClients(t *testing.T) {
	addr := proxyServer(t, 1, "")
	_, _ = proxyDial(t, addr, "PROXY ")
	conn, reader := proxyDial(t, addr, proxyHeader("100.64.2.2"))
	if status := proxyRequest(t, conn, reader); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestIPv6ProxyPeer(t *testing.T) {
	addr := proxyServer(t, 10, "")
	conn, reader := proxyDial(t, addr, "PROXY TCP6 fd7a:115c:a1e0::1 fd7a:115c:a1e0::2 1234 2447\r\n")
	if status := proxyRequest(t, conn, reader); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestNonLoopbackWithoutProxyHeaderStillWorks(t *testing.T) {
	addr := proxyServer(t, 10, "192.0.2.5")
	conn, reader := proxyDial(t, addr, "")
	if status := proxyRequest(t, conn, reader); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

func TestListenerLogsRejectedPreface(t *testing.T) {
	var logs lockedLog
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	addr := proxyServer(t, 10, "192.0.2.5")
	conn, reader := proxyDial(t, addr, proxyHeader("100.64.1.2"))
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("untrusted preface accepted")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatal(err)
	}
	got := logs.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "remote_ip=192.0.2.5") || !strings.Contains(got, "err=") {
		t.Fatalf("preface rejection log = %q", got)
	}
}

func TestListenerLogsPendingPrefaceCapacity(t *testing.T) {
	var logs lockedLog
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	addr := proxyServer(t, 10, "")
	for range 128 {
		proxyDial(t, addr, "PROXY ")
	}
	conn, reader := proxyDial(t, addr, "PROXY ")
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("connection above pending limit was accepted")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("pending connection was not shed: %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "remote_ip=127.0.0.1") || !strings.Contains(got, "pending") {
		t.Fatalf("pending limit log = %q", got)
	}
}

func TestListenerClosesStalledPreface(t *testing.T) {
	var logs lockedLog
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	addr := proxyServer(t, 10, "")
	conn, reader := proxyDial(t, addr, "PROXY TCP4 100.64.")
	if err := conn.SetReadDeadline(time.Now().Add(6 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("stalled preface remained open")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("stalled preface was not closed: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "remote_ip=127.0.0.1") {
		t.Fatalf("stalled preface log = %q", got)
	}
}

func TestListenerLogsEmptyProbeAtDebugAndPartialPrefacesAtWarning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		preface string
		level   string
	}{
		{"empty probe", "", "DEBUG"},
		{"short partial preface", "PRO", "WARN"},
		{"partial proxy header", "PROXY TCP4 100.64.", "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs lockedLog
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(previous)
			addr := proxyServer(t, 10, "")
			conn, reader := proxyDial(t, addr, tc.preface)
			tcp, ok := conn.(*net.TCPConn)
			if !ok {
				t.Fatal("connection is not TCP")
			}
			if err := tcp.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.ReadByte(); err == nil {
				t.Fatal("connection remained open")
			} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
				t.Fatal(err)
			}
			got := logs.String()
			if !strings.Contains(got, "level="+tc.level) || !strings.Contains(got, "remote_ip=127.0.0.1") {
				t.Fatalf("preface log = %q, want %s with remote IP", got, tc.level)
			}
			if tc.level == "DEBUG" && strings.Contains(got, "level=WARN") {
				t.Fatalf("empty probe logged warning: %q", got)
			}
		})
	}
}
