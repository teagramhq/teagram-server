package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestAllowedRemote(t *testing.T) {
	gateway := netip.MustParseAddr("172.22.0.1")
	for _, test := range []struct {
		name   string
		remote string
		want   bool
	}{
		{name: "loopback", remote: "127.0.0.1"},
		{name: "tailnet peer", remote: "100.124.236.10"},
		{name: "docker host proxy", remote: "172.22.0.1", want: true},
		{name: "other default network container", remote: "172.22.0.8"},
		{name: "private LAN", remote: "192.168.1.10"},
		{name: "public IPv4", remote: "8.8.8.8"},
		{name: "IPv6", remote: "fd7a:115c:a1e0::1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := allowedRemote(netip.MustParseAddr(test.remote), gateway); got != test.want {
				t.Fatalf("allowedRemote(%s) = %t, want %t", test.remote, got, test.want)
			}
		})
	}
}

func TestDefaultGateway(t *testing.T) {
	contents := "Iface Destination Gateway Flags RefCnt Use Metric Mask\n" +
		"eth0 00000000 010016AC 0003 0 0 0 00000000\n" +
		"eth1 00000000 0100000A 0001 0 0 10 00000000\n"
	got, err := parseDefaultGateway(contents)
	if err != nil {
		t.Fatal(err)
	}
	if want := netip.MustParseAddr("172.22.0.1"); got != want {
		t.Fatalf("defaultGateway() = %s, want %s", got, want)
	}
}

func TestDefaultGatewayRejectsMissingRoute(t *testing.T) {
	if _, err := parseDefaultGateway("Iface Destination Gateway Flags\n"); err == nil {
		t.Fatal("defaultGateway() succeeded without a default route")
	}
}

func TestHealthCheckProbesForwarderAndOptionalUpstream(t *testing.T) {
	forwarder := listenTCP(t)
	defer closeTest(t, forwarder)
	unavailable := listenTCP(t)
	upstreamAddress := unavailable.Addr().String()
	if err := unavailable.Close(); err != nil {
		t.Fatal(err)
	}

	if err := checkHealth(forwarder.Addr().String(), upstreamAddress, false); err != nil {
		t.Fatalf("health check with disabled Bot API listener: %v", err)
	}
	if err := checkHealth(forwarder.Addr().String(), upstreamAddress, true); err == nil {
		t.Fatal("health check passed with the enabled upstream unavailable")
	}

	upstream := listenTCP(t)
	defer closeTest(t, upstream)
	if err := checkHealth(forwarder.Addr().String(), upstream.Addr().String(), true); err != nil {
		t.Fatalf("health check with enabled Bot API listener: %v", err)
	}
	if err := checkHealth(upstreamAddress, upstream.Addr().String(), false); err == nil {
		t.Fatal("health check passed with the forwarder unavailable")
	}
}

func TestConnectionLimitRejectsExcessClient(t *testing.T) {
	active := make(chan struct{}, 1)
	first, firstPeer := net.Pipe()
	defer closeTest(t, firstPeer)
	if !reserveConnection(active, first) {
		t.Fatal("first client was rejected")
	}
	defer func() {
		<-active
		closeQuietly(first)
	}()

	second, secondPeer := net.Pipe()
	defer closeTest(t, secondPeer)
	if reserveConnection(active, second) {
		t.Fatal("client over the connection limit was admitted")
	}
	if _, err := secondPeer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("rejected client read error = %v, want EOF", err)
	}
}

func TestAcceptFailureBacksOffBeforeRetry(t *testing.T) {
	listener := &sequenceListener{errors: []error{errors.New("temporary accept failure"), net.ErrClosed}}
	backoff := 20 * time.Millisecond
	started := time.Now()
	serve(listener, netip.MustParseAddr("172.22.0.1"), backoff)
	if elapsed := time.Since(started); elapsed < backoff {
		t.Fatalf("serve retried after %s, want at least %s", elapsed, backoff)
	}
	if listener.calls != 2 {
		t.Fatalf("Accept calls = %d, want 2", listener.calls)
	}
}

func TestForwardRelaysAndPropagatesHalfClose(t *testing.T) {
	upstream := listenTCP(t)
	defer closeTest(t, upstream)
	serverDone := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer closeQuietly(conn)
		reader := bufio.NewReader(conn)
		header, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(header, "PROXY TCP4 ") {
			serverDone <- errors.New("missing PROXY header")
			return
		}
		request, err := io.ReadAll(reader)
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := conn.Write(append([]byte("echo:"), request...)); err != nil {
			serverDone <- err
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			if err := tcp.CloseWrite(); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()

	client, forwarder := tcpPair(t)
	forwardDone := make(chan struct{})
	go func() {
		forwardTo(forwarder, upstream.Addr().String(), time.Second)
		close(forwardDone)
	}()
	defer closeTest(t, client)
	if _, err := client.Write([]byte("PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\r\nlong-poll")); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(response), "echo:long-poll"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-forwardDone:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not finish after both half-closes")
	}
}

func TestForwardClosesAnIdleConnection(t *testing.T) {
	upstream := listenTCP(t)
	defer closeTest(t, upstream)
	serverDone := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer closeQuietly(conn)
		_, err = io.Copy(io.Discard, conn)
		serverDone <- err
	}()
	client, forwarder := tcpPair(t)
	defer closeTest(t, client)
	done := make(chan struct{})
	go func() {
		forwardTo(forwarder, upstream.Addr().String(), 40*time.Millisecond)
		close(done)
	}()
	if _, err := client.Write([]byte("PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\r\nrequest")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("idle connection read error = %v, want EOF", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwarder did not close the idle connection")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("upstream fixture copy failed: %v", err)
	}
}

func TestForwardLogsUpstreamDialError(t *testing.T) {
	unavailable := listenTCP(t)
	upstreamAddress := unavailable.Addr().String()
	if err := unavailable.Close(); err != nil {
		t.Fatal(err)
	}
	client, forwarder := tcpPair(t)
	defer closeTest(t, client)
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previousLogger)

	if _, err := io.WriteString(client, "PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\r\n"); err != nil {
		t.Fatal(err)
	}
	forwardTo(forwarder, upstreamAddress, time.Second)
	if got := logs.String(); !strings.Contains(got, "Bot API upstream unavailable") || !strings.Contains(got, "err=") {
		t.Fatalf("upstream dial failure log = %q, want contextual error", got)
	}
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener := listenTCP(t)
	defer closeTest(t, listener)
	clientConn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, ok := clientConn.(*net.TCPConn)
	if !ok {
		t.Fatal("client connection is not TCP")
	}
	forwarderConn, err := listener.Accept()
	if err != nil {
		closeQuietly(client)
		t.Fatal(err)
	}
	forwarder, ok := forwarderConn.(*net.TCPConn)
	if !ok {
		closeQuietly(client)
		closeQuietly(forwarderConn)
		t.Fatal("forwarder connection is not TCP")
	}
	return client, forwarder
}

func closeTest(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Errorf("close fixture: %v", err)
	}
}

type sequenceListener struct {
	errors []error
	calls  int
}

func (l *sequenceListener) Accept() (net.Conn, error) {
	if l.calls >= len(l.errors) {
		return nil, net.ErrClosed
	}
	err := l.errors[l.calls]
	l.calls++
	return nil, err
}

func (l *sequenceListener) Close() error { return nil }

func (l *sequenceListener) Addr() net.Addr { return &net.TCPAddr{} }
