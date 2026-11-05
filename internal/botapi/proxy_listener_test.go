package botapi_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
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

func TestProxyPeersHaveIndependentAuthBuckets(t *testing.T) {
	addr := proxyServer(t, 10, "")
	a, ar := proxyDial(t, addr, proxyHeader("100.64.1.2"))
	for i := range 20 {
		if status := proxyRequest(t, a, ar); status != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want 401", i, status)
		}
	}
	if status := proxyRequest(t, a, ar); status != http.StatusTooManyRequests {
		t.Fatalf("exhausted peer status = %d, want 429", status)
	}
	b, br := proxyDial(t, addr, proxyHeader("100.64.2.2"))
	if status := proxyRequest(t, b, br); status != http.StatusUnauthorized {
		t.Fatalf("other peer status = %d, want 401", status)
	}
}

func TestProxyPeersHaveIndependentConnectionCaps(t *testing.T) {
	addr := proxyServer(t, 1, "")
	a, ar := proxyDial(t, addr, proxyHeader("100.64.1.2"))
	if status := proxyRequest(t, a, ar); status != http.StatusUnauthorized {
		t.Fatalf("first status = %d", status)
	}
	b, br := proxyDial(t, addr, proxyHeader("100.64.2.2"))
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
