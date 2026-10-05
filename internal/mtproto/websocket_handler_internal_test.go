package mtproto

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type shutdownTestListener struct{}

func (shutdownTestListener) Accept() (net.Conn, error) { return nil, errors.New("unused") }
func (shutdownTestListener) Close() error              { return nil }
func (shutdownTestListener) Addr() net.Addr            { return shutdownTestAddr("unused") }

type shutdownTestAddr string

func (a shutdownTestAddr) Network() string { return string(a) }
func (a shutdownTestAddr) String() string  { return string(a) }

func TestWebSocketWaitTracksHandlerAfterSocketClose(t *testing.T) {
	listener := newWebSocketListener(shutdownTestListener{}, nil)
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
