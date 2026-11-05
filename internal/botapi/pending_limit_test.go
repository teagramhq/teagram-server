package botapi_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/botapi"
	"github.com/teagramhq/teagram-server/internal/store"
)

// Map distinct loopback socket sources to non-loopback peers without requiring
// a tailnet interface in the test environment.
type mappedPeerListener struct{ net.Listener }

func (l mappedPeerListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		if closeErr := c.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, err
	}
	switch host {
	case "127.0.0.2":
		return &overrideConn{Conn: c, remote: "192.0.2.2"}, nil
	case "127.0.0.3":
		return &overrideConn{Conn: c, remote: "192.0.2.3"}, nil
	default:
		if closeErr := c.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, fmt.Errorf("unexpected source %s", host)
	}
}

func pendingTestServer(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := botapi.NewServer("", newFakeStore(), slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(botapi.LimitConnectionsPerIP(mappedPeerListener{ln}, 2)) }()
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

func pendingDial(t *testing.T, addr, source string) (net.Conn, *bufio.Reader) {
	t.Helper()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
	c, err := dialer.DialContext(context.Background(), "tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

func TestDirectPeerCannotExhaustPendingPrefaces(t *testing.T) {
	addr := pendingTestServer(t)
	pendingDial(t, addr, "127.0.0.2")
	pendingDial(t, addr, "127.0.0.2")
	_, rejected := pendingDial(t, addr, "127.0.0.2")
	if _, err := rejected.ReadByte(); err == nil {
		t.Fatal("connection above per-peer pending cap remained open")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("excess connection was not promptly closed: %v", err)
	}
	other, reader := pendingDial(t, addr, "127.0.0.3")
	if status := proxyRequest(t, other, reader); status != http.StatusUnauthorized {
		t.Fatalf("other peer status = %d, want 401", status)
	}
}

func TestDirectPeerCapIncludesPendingAndActive(t *testing.T) {
	addr := pendingTestServer(t)
	active, reader := pendingDial(t, addr, "127.0.0.2")
	if status := proxyRequest(t, active, reader); status != http.StatusUnauthorized {
		t.Fatalf("first status = %d", status)
	}
	pendingDial(t, addr, "127.0.0.2")
	_, rejected := pendingDial(t, addr, "127.0.0.2")
	if _, err := rejected.ReadByte(); err == nil {
		t.Fatal("connection above combined cap remained open")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("excess connection was not promptly closed: %v", err)
	}
}

func TestDirectPendingReservationReleasedOnDisconnect(t *testing.T) {
	addr := pendingTestServer(t)
	first, firstReader := pendingDial(t, addr, "127.0.0.2")
	pendingDial(t, addr, "127.0.0.2")
	tcp, ok := first.(*net.TCPConn)
	if !ok {
		t.Fatal("connection is not TCP")
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := firstReader.ReadByte(); err == nil {
		t.Fatal("idle connection remained open")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatal(err)
	}
	replacement, reader := pendingDial(t, addr, "127.0.0.2")
	if status := proxyRequest(t, replacement, reader); status != http.StatusUnauthorized {
		t.Fatalf("replacement status = %d, want 401", status)
	}
}
