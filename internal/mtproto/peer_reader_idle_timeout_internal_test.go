package mtproto

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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

type closeErrorPeerTransport struct {
	closed   chan struct{}
	closeErr error
	recvErr  error
	once     sync.Once
}

func (*closeErrorPeerTransport) Send(context.Context, *bin.Buffer) error { return nil }

func (t *closeErrorPeerTransport) Recv(ctx context.Context, _ *bin.Buffer) error {
	if t.recvErr != nil {
		return t.recvErr
	}
	select {
	case <-t.closed:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *closeErrorPeerTransport) Close() error {
	t.once.Do(func() { close(t.closed) })
	return t.closeErr
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

	_, _, err := reader.next(context.Background(), time.Time{}, true)
	if !errors.Is(err, errIdleReadTimeout) {
		t.Fatalf("peer read error = %v, want idle-timeout classification", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("peer read error = %v, want deadline cause", err)
	}
	reader.join()
}

func TestPeerFrameReaderCloseErrorsIncludeConnectionID(t *testing.T) {
	for _, tc := range []struct {
		name           string
		peerDisconnect bool
		message        string
	}{
		{name: "peer disconnect", peerDisconnect: true, message: "close transport after peer disconnect"},
		{name: "read deadline", message: "close connection at read deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&output, nil))
			server := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, logger)
			server.readTimeout = 10 * time.Millisecond
			transport := &closeErrorPeerTransport{
				closed:   make(chan struct{}),
				closeErr: errors.New("injected close failure"),
			}
			if tc.peerDisconnect {
				transport.recvErr = io.EOF
			}
			const connectionID = "peer-reader-test-id"
			conn := &Conn{transport: transport, log: logger.With("connection_id", connectionID)}
			reader := &peerFrameReader{server: server, transport: transport, conn: conn}

			if tc.peerDisconnect {
				reader.prefetch()
				if _, _, err := reader.next(context.Background(), time.Time{}, false); !errors.Is(err, io.EOF) {
					t.Fatalf("peer read error = %v, want EOF", err)
				}
			} else {
				if _, _, err := reader.next(context.Background(), time.Time{}, true); !errors.Is(err, errIdleReadTimeout) {
					t.Fatalf("peer read error = %v, want idle timeout", err)
				}
			}
			reader.join()

			logOutput := output.String()
			if !strings.Contains(logOutput, `msg="`+tc.message+`"`) {
				t.Fatalf("close failure record %q missing from logs:\n%s", tc.message, logOutput)
			}
			if !strings.Contains(logOutput, "connection_id="+connectionID) {
				t.Errorf("%s record omits connection_id %q:\n%s", tc.message, connectionID, logOutput)
			}
		})
	}
}
