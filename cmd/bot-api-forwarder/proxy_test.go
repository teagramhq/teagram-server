package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/botapi"
)

func TestGatewayOnly(t *testing.T) {
	gateway := netip.MustParseAddr("172.22.0.1")
	if allowedRemote(netip.MustParseAddr("100.64.1.2"), gateway) {
		t.Fatal("direct tailnet connection was allowed")
	}
	if !allowedRemote(gateway, gateway) {
		t.Fatal("gateway connection was rejected")
	}
}

func TestForwardRequiresValidProxyBeforeDial(t *testing.T) {
	for _, input := range []string{"", "GET / HTTP/1.1\r\n\r\n", "PROXY TCP4 999.0.0.1 100.64.0.1 1234 2447\r\n", "PROXY " + strings.Repeat("x", 110) + "\r\n"} {
		t.Run(fmt.Sprintf("input=%q", input), func(t *testing.T) {
			upstream := proxyTestTCPListener(t)
			defer closeTest(t, upstream)
			client, fwd := tcpPair(t)
			defer closeTest(t, client)
			done := make(chan struct{})
			go func() { forwardTo(fwd, upstream.Addr().String(), time.Second); close(done) }()
			if _, err := io.WriteString(client, input); err != nil {
				t.Fatal(err)
			}
			if input == "" {
				if err := client.CloseWrite(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("forwarder did not reject invalid header")
			}
			if err := upstream.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			conn, err := upstream.Accept()
			if err == nil {
				closeQuietly(conn)
				t.Fatal("upstream was dialled before header validation")
			}
			if ne, ok := errors.AsType[net.Error](err); !ok || !ne.Timeout() {
				t.Fatal(err)
			}
		})
	}
}

func TestForwardPreservesProxyHeaderAndPayload(t *testing.T) {
	upstream := listenTCP(t)
	defer closeTest(t, upstream)
	client, fwd := tcpPair(t)
	defer closeQuietly(client)
	done := make(chan struct{})
	go func() { forwardTo(fwd, upstream.Addr().String(), time.Second); close(done) }()
	header := "PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\r\n"
	if _, err := io.WriteString(client, header+"request"); err != nil {
		t.Fatal(err)
	}
	conn, err := upstream.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(conn)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil || line != header {
		t.Fatalf("header = %q, err = %v", line, err)
	}
	payload := make([]byte, len("request"))
	if _, err := io.ReadFull(reader, payload); err != nil || string(payload) != "request" {
		t.Fatalf("payload = %q, err = %v", payload, err)
	}
	closeQuietly(conn)
	closeQuietly(client)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not finish")
	}
}

func TestForwardTimesOutIncompleteHeaderBeforeDial(t *testing.T) {
	upstream := proxyTestTCPListener(t)
	defer closeTest(t, upstream)
	client, fwd := tcpPair(t)
	defer closeTest(t, client)
	done := make(chan struct{})
	go func() { forwardTo(fwd, upstream.Addr().String(), time.Second); close(done) }()
	if _, err := io.WriteString(client, "PROXY TCP4 100.64."); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("incomplete header did not time out")
	}
	if err := upstream.SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	conn, err := upstream.Accept()
	if err == nil {
		closeQuietly(conn)
		t.Fatal("upstream was dialled for incomplete header")
	}
	if ne, ok := errors.AsType[net.Error](err); !ok || !ne.Timeout() {
		t.Fatal(err)
	}
}

func TestForwarderAndBotListenerPreservePeer(t *testing.T) {
	upstream := listenTCP(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Peer", r.RemoteAddr)
	}), ReadHeaderTimeout: time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(botapi.LimitConnectionsPerIP(upstream, 1)) }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Error(err)
		}
		if err := <-serverDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})
	client, fwd := tcpPair(t)
	defer closeQuietly(client)
	done := make(chan struct{})
	go func() { forwardTo(fwd, upstream.Addr().String(), time.Second); close(done) }()
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(client, "PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\r\nGET / HTTP/1.1\r\nHost: example\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if peer := resp.Header.Get("X-Test-Peer"); peer != "100.64.1.2:1234" {
		t.Fatalf("remote address = %q", peer)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not finish")
	}
}

func proxyTestTCPListener(t *testing.T) *net.TCPListener {
	t.Helper()
	listener := listenTCP(t)
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		closeQuietly(listener)
		t.Fatal("listener is not TCP")
	}
	return tcp
}

func TestForwardLogsInvalidPrefaceAtWarning(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	client, fwd := tcpPair(t)
	defer closeTest(t, client)
	peer, _, err := net.SplitHostPort(client.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(client, "BAD REQUEST\r\n"); err != nil {
		t.Fatal(err)
	}
	forwardTo(fwd, "127.0.0.1:1", time.Second)
	got := logs.String()
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "remote_ip="+peer) || !strings.Contains(got, "err=") {
		t.Fatalf("invalid preface log = %q", got)
	}
}
