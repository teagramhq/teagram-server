package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWithRPCDeadlineBoundsCall(t *testing.T) {
	const timeout = 25 * time.Millisecond
	err := withRPCDeadline(context.Background(), timeout, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("RPC context has no deadline")
		}
		if remaining := time.Until(deadline); remaining <= 0 || remaining > timeout {
			return errors.New("RPC deadline is outside the requested timeout")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("withRPCDeadline() error = %v, want context deadline exceeded", err)
	}
}

func TestServeProxyV2EchoAcceptsNewConnectionWhileEarlierStaysOpen(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		observed []string
		err      error
	}
	done := make(chan result, 1)
	go func() {
		observed, err := serveProxyV2Echo(listener, 2)
		done <- result{observed, err}
	}()

	send := func(source [4]byte) {
		t.Helper()
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		header := []byte("\r\n\r\n\x00\r\nQUIT\n\x21\x11\x00\x0c")
		header = append(header, source[:]...)
		header = append(header, 198, 18, 132, 11, 0x9c, 0x40, 0x09, 0x8e)
		request := "GET /apiws HTTP/1.1\r\nHost: example.test\r\nConnection: keep-alive\r\n\r\n"
		if _, err := conn.Write(append(header, request...)); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
	}
	// HAProxy keeps the first backend connection idle on the session and
	// opens a new one for a request carrying a different source address.
	send([4]byte{203, 0, 113, 101})
	send([4]byte{203, 0, 114, 102})

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("serveProxyV2Echo() error = %v", got.err)
		}
		if strings.Join(got.observed, ",") != "203.0.113.101,203.0.114.102" {
			t.Fatalf("observed = %v", got.observed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveProxyV2Echo() did not finish")
	}
}
