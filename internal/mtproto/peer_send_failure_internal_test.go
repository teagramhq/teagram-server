package mtproto

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/clock"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
)

type peerSendFailureTransport struct {
	send   func(context.Context) error
	closed atomic.Bool
	calls  atomic.Int32
}

func (c *peerSendFailureTransport) Send(ctx context.Context, _ *bin.Buffer) error {
	c.calls.Add(1)
	if c.send != nil {
		return c.send(ctx)
	}
	return nil
}

func (*peerSendFailureTransport) Recv(context.Context, *bin.Buffer) error { return io.EOF }

func (c *peerSendFailureTransport) Close() error {
	c.closed.Store(true)
	return nil
}

type peerSendFailureRandom struct{ err error }

func (r peerSendFailureRandom) Read([]byte) (int, error) { return 0, r.err }

func newPeerSendFailureConn(t *testing.T, transport *peerSendFailureTransport, cipher crypto.Cipher, budget *peerCancelBudget) *Conn {
	t.Helper()
	conn := newConn(transport, cipher, proto.NewMessageIDGen(clock.System.Now), clock.System, time.Second, nil)
	conn.setKey(crypto.Key{1, 2, 3, 4}.WithID())
	conn.setSession(42)
	conn.setOwner(71)
	conn.peerCancels = budget
	return conn
}

func beginPeerSendFailureRPC(t *testing.T, conn *Conn, userID int64) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	active := &activePeerRPC{userID: userID, ctx: ctx, cancel: cancel}
	if !conn.beginActiveRPC(active) {
		cancel()
		t.Fatal("active RPC was not admitted")
	}
	t.Cleanup(func() {
		conn.finishActiveRPC(active)
		cancel()
	})
	return ctx, cancel
}

func assertPeerSendAllowanceAvailable(t *testing.T, budget *peerCancelBudget, userID int64) {
	t.Helper()
	reservation, denial := budget.reserve(userID)
	if denial != peerCancelAllowed {
		t.Fatalf("peer cancellation allowance denied: %q", denial)
	}
	reservation.release()
}

func TestFailedPushCleanupPreservesDeniedPeerCancellation(t *testing.T) {
	budget := testPeerCancelBudget()
	spent, denial := budget.reserve(71)
	if denial != peerCancelAllowed {
		t.Fatalf("initial peer cancellation reservation denied: %q", denial)
	}
	spent.commit()

	transport := &peerSendFailureTransport{send: func(context.Context) error { return io.ErrClosedPipe }}
	conn := newPeerSendFailureConn(t, transport, crypto.NewServerCipher(crypto.DefaultRand()), budget)
	rpcCtx, _ := beginPeerSendFailureRPC(t, conn, 71)

	if _, err := conn.PushTo(context.Background(), 71, &mt.Pong{PingID: 1}, 0); err == nil {
		t.Fatal("failed peer write returned no error")
	}
	if err := conn.CloseAfterPushFailure(); err != nil {
		t.Fatalf("failed-push cleanup: %v", err)
	}
	if !transport.closed.Load() {
		t.Fatal("failed-push cleanup did not leave the transport closed")
	}
	if rpcCtx.Err() != nil {
		t.Fatalf("capped peer disconnect canceled the active RPC during cleanup: %v", rpcCtx.Err())
	}
	if conn.closeSource.Load() != closeSourcePeer || !conn.peerLost.Load() {
		t.Fatalf("close state = source %d, peer lost %t; want peer provenance", conn.closeSource.Load(), conn.peerLost.Load())
	}
	if _, denial := budget.reserve(71); denial != peerCancelDeniedUserWindow {
		t.Fatalf("failed-push cleanup changed capped user allowance: %q", denial)
	}
}

func TestConfirmedPeerWriteDisconnectCancelsAndCharges(t *testing.T) {
	budget := testPeerCancelBudget()
	transport := &peerSendFailureTransport{send: func(context.Context) error { return io.ErrClosedPipe }}
	conn := newPeerSendFailureConn(t, transport, crypto.NewServerCipher(crypto.DefaultRand()), budget)
	rpcCtx, _ := beginPeerSendFailureRPC(t, conn, 71)

	if _, err := conn.PushTo(context.Background(), 71, &mt.Pong{PingID: 4}, 0); err == nil {
		t.Fatal("failed peer write returned no error")
	}
	if !transport.closed.Load() {
		t.Fatal("confirmed peer disconnect did not close the transport")
	}
	if rpcCtx.Err() == nil {
		t.Fatal("confirmed peer disconnect did not cancel the active RPC")
	}
	if conn.closeSource.Load() != closeSourcePeer || !conn.peerLost.Load() {
		t.Fatalf("close state = source %d, peer lost %t; want peer provenance", conn.closeSource.Load(), conn.peerLost.Load())
	}
	if _, denial := budget.reserve(71); denial != peerCancelDeniedUserWindow {
		t.Fatalf("applied peer cancellation allowance = %q, want user-window exhaustion", denial)
	}
}

func TestLocalEncryptionFailureIsServerOwned(t *testing.T) {
	budget := testPeerCancelBudget()
	transport := &peerSendFailureTransport{}
	conn := newPeerSendFailureConn(t, transport, crypto.NewServerCipher(peerSendFailureRandom{err: errors.New("random source failed")}), budget)
	rpcCtx, _ := beginPeerSendFailureRPC(t, conn, 71)

	if _, err := conn.PushTo(context.Background(), 71, &mt.Pong{PingID: 2}, 0); err == nil {
		t.Fatal("encryption failure returned no error")
	}
	if conn.closeSource.Load() != closeSourceServer || conn.peerLost.Load() {
		t.Fatalf("encryption failure close state = source %d, peer lost %t; want server-owned", conn.closeSource.Load(), conn.peerLost.Load())
	}
	if rpcCtx.Err() == nil {
		t.Fatal("server-owned encryption failure did not stop the active RPC")
	}
	if !transport.closed.Load() {
		t.Fatal("encryption failure did not close the transport")
	}
	if calls := transport.calls.Load(); calls != 0 {
		t.Fatalf("transport sends = %d, want none after encryption failure", calls)
	}
	assertPeerSendAllowanceAvailable(t, budget, 71)
}

func TestCallerDeadlineWriteTimeoutIsNotPeerDisconnect(t *testing.T) {
	budget := testPeerCancelBudget()
	transport := &peerSendFailureTransport{send: func(ctx context.Context) error {
		<-ctx.Done()
		return &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}}
	conn := newPeerSendFailureConn(t, transport, crypto.NewServerCipher(crypto.DefaultRand()), budget)
	rpcCtx, _ := beginPeerSendFailureRPC(t, conn, 71)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := conn.PushTo(ctx, 71, &mt.Pong{PingID: 3}, 0); err == nil {
		t.Fatal("caller-deadline write timeout returned no error")
	}
	if conn.closeSource.Load() != closeSourceServer || conn.peerLost.Load() {
		t.Fatalf("caller-deadline close state = source %d, peer lost %t; want server-owned", conn.closeSource.Load(), conn.peerLost.Load())
	}
	if rpcCtx.Err() != nil {
		t.Fatalf("caller-deadline write timeout canceled active RPC: %v", rpcCtx.Err())
	}
	if !transport.closed.Load() {
		t.Fatal("caller-deadline write timeout did not close the transport")
	}
	assertPeerSendAllowanceAvailable(t, budget, 71)
}
