package mtproto

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/exchange"
)

type idleTimeoutPeerTransport struct {
	closed chan struct{}
	once   sync.Once
}

func (t *idleTimeoutPeerTransport) Send(context.Context, *bin.Buffer) error { return nil }

func (t *idleTimeoutPeerTransport) Recv(context.Context, *bin.Buffer) error {
	<-t.closed
	return io.EOF
}

func (t *idleTimeoutPeerTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func TestPeerFrameReaderClassifiesIdleReadTimeout(t *testing.T) {
	transport := &idleTimeoutPeerTransport{closed: make(chan struct{})}
	server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, nil)
	server.readTimeout = 10 * time.Millisecond
	conn := &Conn{transport: transport}
	reader := &peerFrameReader{server: server, transport: transport, conn: conn}

	_, err := reader.next(context.Background(), time.Time{}, true)
	if !errors.Is(err, errIdleReadTimeout) {
		t.Fatalf("peer read error = %v, want idle-timeout classification", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("peer read error = %v, want deadline cause", err)
	}
	reader.join()
}
