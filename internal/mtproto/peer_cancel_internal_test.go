package mtproto

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"
)

type peerCountingTransport struct {
	transport.Conn

	active   atomic.Int32
	maximum  atomic.Int32
	receives atomic.Int32
}

func (c *peerCountingTransport) Recv(ctx context.Context, b *bin.Buffer) error {
	c.receives.Add(1)
	active := c.active.Add(1)
	for maximum := c.maximum.Load(); active > maximum; maximum = c.maximum.Load() {
		if c.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer c.active.Add(-1)
	return c.Conn.Recv(ctx, b)
}

func testPeerCancelBudget() *peerCancelBudget {
	now := time.Unix(1_800_000_200, 0)
	return newPeerCancelBudget(func() time.Time { return now }, 8, peerCancelUserWindow, peerCancelGlobalRefillPeriod, peerCancelGlobalBurst)
}

func TestPeerDisconnectCancelsAndChargesOnlyAnActiveRPC(t *testing.T) {
	budget := testPeerCancelBudget()
	conn := &Conn{peerCancels: budget}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := &activePeerRPC{userID: 31, cancel: cancel}
	if !conn.beginActiveRPC(active) {
		t.Fatal("beginActiveRPC refused a live connection")
	}

	conn.observePeerDisconnect()
	if !errorsIsCanceled(ctx) {
		t.Fatalf("active RPC context error = %v, want context.Canceled", ctx.Err())
	}
	if conn.closeSource.Load() != closeSourcePeer || !conn.peerLost.Load() {
		t.Fatalf("peer close state = source %d, lost %t", conn.closeSource.Load(), conn.peerLost.Load())
	}
	if _, denial := budget.reserve(31); denial != peerCancelDeniedUserWindow {
		t.Fatalf("post-cancel user reservation = %q, want user window", denial)
	}
	conn.finishActiveRPC(active)

	idle := &Conn{peerCancels: budget}
	idle.observePeerDisconnect()
	if _, denial := budget.reserve(32); denial != peerCancelAllowed {
		t.Fatalf("idle disconnect used global allowance: next reservation = %q", denial)
	}
}

func TestServerCloseCancelsWithoutSpendingPeerAllowance(t *testing.T) {
	budget := testPeerCancelBudget()
	conn := &Conn{peerCancels: budget, transport: newDrainWriteTestTransport()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := &activePeerRPC{userID: 41, cancel: cancel}
	if !conn.beginActiveRPC(active) {
		t.Fatal("beginActiveRPC refused a live connection")
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("server-owned connection close: %v", err)
	}
	if !errorsIsCanceled(ctx) {
		t.Fatalf("active RPC context error = %v, want context.Canceled", ctx.Err())
	}
	if conn.closeSource.Load() != closeSourceServer || conn.peerLost.Load() {
		t.Fatalf("server close state = source %d, peer lost %t", conn.closeSource.Load(), conn.peerLost.Load())
	}
	if _, denial := budget.reserve(41); denial != peerCancelAllowed {
		t.Fatalf("server close consumed peer allowance: next reservation = %q", denial)
	}
	conn.finishActiveRPC(active)
}

func TestPeerCancellationBudgetIsSharedAcrossConnectionsAndKeys(t *testing.T) {
	budget := testPeerCancelBudget()
	first := &Conn{peerCancels: budget}
	first.authKeyID.Store(101)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstRPC := &activePeerRPC{userID: 52, ctx: firstCtx, cancel: cancelFirst}
	if !first.beginActiveRPC(firstRPC) {
		t.Fatal("first connection refused a live RPC")
	}
	first.observePeerDisconnect()
	if !errorsIsCanceled(firstCtx) {
		t.Fatal("first connection's active RPC was not canceled")
	}
	first.finishActiveRPC(firstRPC)

	second := &Conn{peerCancels: budget}
	second.authKeyID.Store(202)
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	secondRPC := &activePeerRPC{userID: 52, ctx: secondCtx, cancel: cancelSecond}
	if !second.beginActiveRPC(secondRPC) {
		t.Fatal("second connection refused a live RPC")
	}
	second.observePeerDisconnect()
	if secondCtx.Err() != nil {
		t.Fatalf("a second auth key refreshed the user's cancellation allowance: %v", secondCtx.Err())
	}
	second.finishActiveRPC(secondRPC)
}

func TestServerCloseWinsAgainstReservedPeerCancellation(t *testing.T) {
	budget := testPeerCancelBudget()
	conn := &Conn{peerCancels: budget}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := &activePeerRPC{userID: 45, ctx: ctx, cancel: cancel}
	if !conn.beginActiveRPC(active) {
		t.Fatal("beginActiveRPC refused a live connection")
	}

	reservation, denial := budget.reserve(45)
	if denial != peerCancelAllowed {
		t.Fatalf("peer reservation denial = %q, want allowed", denial)
	}
	active.cancelByServer()
	if active.cancelByPeer() {
		t.Fatal("peer cancellation applied after server close had already canceled the RPC")
	}
	reservation.release()

	if !errorsIsCanceled(ctx) {
		t.Fatalf("RPC context error = %v, want context.Canceled", ctx.Err())
	}
	if _, denial := budget.reserve(45); denial != peerCancelAllowed {
		t.Fatalf("server-winning race spent peer allowance: next reservation = %q", denial)
	}
	conn.finishActiveRPC(active)
}

func TestPeerDisconnectDoesNotChargeAnAlreadyExpiredRPC(t *testing.T) {
	budget := testPeerCancelBudget()
	conn := &Conn{peerCancels: budget}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	active := &activePeerRPC{userID: 46, ctx: ctx, cancel: cancel}
	if !conn.beginActiveRPC(active) {
		t.Fatal("beginActiveRPC refused a live connection")
	}

	conn.observePeerDisconnect()
	if _, denial := budget.reserve(46); denial != peerCancelAllowed {
		t.Fatalf("already-canceled RPC spent peer allowance: next reservation = %q", denial)
	}
	conn.finishActiveRPC(active)
}

func TestPeerDisconnectCompletionRaceRefundsUnappliedReservation(t *testing.T) {
	for range 200 {
		budget := testPeerCancelBudget()
		conn := &Conn{peerCancels: budget}
		ctx, cancel := context.WithCancel(context.Background())
		active := &activePeerRPC{userID: 51, cancel: cancel}
		if !conn.beginActiveRPC(active) {
			t.Fatal("beginActiveRPC refused a live connection")
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			conn.observePeerDisconnect()
		}()
		go func() {
			defer wg.Done()
			<-start
			conn.finishActiveRPC(active)
		}()
		close(start)
		wg.Wait()

		canceled := ctx.Err() != nil
		_, denial := budget.reserve(51)
		if !canceled {
			if denial != peerCancelAllowed {
				t.Fatalf("unapplied completion race spent allowance: %q", denial)
			}
		} else if denial != peerCancelDeniedUserWindow {
			t.Fatalf("applied completion race did not retain allowance: %q", denial)
		}
		cancel()
	}
}

func TestPeerDisconnectCancelsActiveRPCOnTCPAndWebSocket(t *testing.T) {
	for _, websocketTransport := range []bool{false, true} {
		name := "tcp"
		if websocketTransport {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			key := shutdownTestAuthKey()
			started := make(chan struct{})
			cancelled := make(chan error, 1)
			completionLive := make(chan bool, 1)
			var servedConn *Conn
			server := New(exchange.PrivateKey{}, 2, &shutdownLookupAuthKeyStore{key: key}, HandlerFunc(func(c *Conn, req *Request) error {
				servedConn = c
				close(started)
				<-req.Ctx.Done()
				completionLive <- req.CompletionCtx.Err() == nil
				cancelled <- req.Ctx.Err()
				return req.Ctx.Err()
			}), nil)
			server.readTimeout = time.Second
			pair := openShutdownTransportPair(t, server, websocketTransport, nil)
			t.Cleanup(pair.close)

			serveDone := make(chan error, 1)
			go func() {
				serveDone <- server.serveConnWithContexts(
					context.Background(), context.Background(), pair.server, netip.Addr{}, nil, nil,
				)
			}()
			if err := pair.client.Send(context.Background(), &bin.Buffer{Buf: shutdownClientFrame(t, key, 42, 1<<32, &tg.HelpGetConfigRequest{})}); err != nil {
				t.Fatalf("send blocked RPC: %v", err)
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("RPC handler did not start")
			}
			if err := pair.client.Close(); err != nil && !isDisconnect(err) {
				t.Fatalf("close peer transport: %v", err)
			}
			select {
			case err := <-cancelled:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("RPC context error = %v, want context.Canceled", err)
				}
				if !<-completionLive {
					t.Fatal("peer disconnect canceled the post-commit completion context")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("peer disconnect did not cancel the active RPC")
			}
			select {
			case err := <-serveDone:
				if err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("serve result = %v, want peer-cancelled RPC", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("serve loop did not return after peer disconnect")
			}
			if servedConn == nil || servedConn.closeSource.Load() != closeSourcePeer {
				t.Fatalf("close provenance = %v, want peer", servedConn)
			}
			if _, denial := server.peerCancels.reserve(7); denial != peerCancelDeniedUserWindow {
				t.Fatalf("applied cancellation budget result = %q, want user window", denial)
			}
		})
	}
}

func TestServerHardCutoffCancelsActiveRPCWithoutPeerAllowance(t *testing.T) {
	for _, websocketTransport := range []bool{false, true} {
		name := "tcp"
		if websocketTransport {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			key := shutdownTestAuthKey()
			entered := make(chan struct{}, 1)
			cancelled := make(chan error, 1)
			completionErr := make(chan error, 1)
			var servedConn *Conn
			server := New(exchange.PrivateKey{}, 2, &shutdownLookupAuthKeyStore{key: key}, HandlerFunc(func(c *Conn, req *Request) error {
				servedConn = c
				entered <- struct{}{}
				<-req.Ctx.Done()
				completionErr <- req.CompletionCtx.Err()
				cancelled <- req.Ctx.Err()
				return req.Ctx.Err()
			}), nil)
			pair := openShutdownTransportPair(t, server, websocketTransport, nil)
			t.Cleanup(pair.close)
			t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
			serverTransport := pair.server
			if websocketTransport {
				serverTransport = webSocketDrainConn{Conn: pair.server, socket: pair.socket, shutdown: server.shutdown}
			}

			serveDone := make(chan error, 1)
			go func() {
				serveDone <- server.serveConnWithContexts(
					context.Background(), server.shutdown.requestCtx, serverTransport, netip.Addr{}, nil, nil,
				)
			}()
			if err := pair.client.Send(context.Background(), &bin.Buffer{Buf: shutdownClientFrame(t, key, 42, 1<<32, &tg.HelpGetConfigRequest{})}); err != nil {
				t.Fatalf("send active RPC: %v", err)
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("RPC handler did not start")
			}

			server.shutdown.beginDrain()
			server.shutdown.cancelReq()
			select {
			case err := <-cancelled:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("RPC context error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("hard cutoff did not cancel the active RPC")
			}
			select {
			case <-serveDone:
			case <-time.After(2 * time.Second):
				t.Fatal("serve loop did not return after hard cutoff")
			}
			if servedConn == nil || servedConn.closeSource.Load() != closeSourceServer || servedConn.peerLost.Load() {
				t.Fatalf("hard-cutoff provenance = %v, want server-owned close", servedConn)
			}
			if err := <-completionErr; !errors.Is(err, context.Canceled) {
				t.Fatalf("hard cutoff did not cancel post-commit completion context: %v (server context: %v, close source: %d)", err, server.shutdown.requestCtx.Err(), servedConn.closeSource.Load())
			}
			if _, denial := server.peerCancels.reserve(7); denial != peerCancelAllowed {
				t.Fatalf("hard cutoff consumed peer allowance: next reservation = %q", denial)
			}
		})
	}
}

func TestDrainPreservesAdmittedRPCAndJoinsPeerReader(t *testing.T) {
	for _, websocketTransport := range []bool{false, true} {
		name := "tcp"
		if websocketTransport {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			key := shutdownTestAuthKey()
			type rpcContexts struct {
				requestDone    <-chan struct{}
				completionDone <-chan struct{}
			}
			entered := make(chan rpcContexts, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
			var servedConn *Conn
			server := New(exchange.PrivateKey{}, 2, &shutdownLookupAuthKeyStore{key: key}, HandlerFunc(func(c *Conn, req *Request) error {
				servedConn = c
				entered <- rpcContexts{requestDone: req.Ctx.Done(), completionDone: req.CompletionCtx.Done()}
				<-release
				return c.SendResult(req, &tg.BoolTrue{})
			}), nil)
			server.shutdown.drainTimeout = 2 * time.Second
			server.shutdown.retirementWindow = 200 * time.Millisecond
			server.shutdown.cleanupReserve = 100 * time.Millisecond
			server.shutdown.retirementSlots = 1
			pair := openShutdownTransportPair(t, server, websocketTransport, nil)
			t.Cleanup(pair.close)
			t.Cleanup(func() {
				releaseHandler()
				stopShutdownTimers(server.shutdown)
			})
			counted := &peerCountingTransport{Conn: pair.server}

			serveDone := make(chan error, 1)
			go func() {
				serveDone <- server.serveConnWithContexts(
					context.Background(), server.shutdown.requestCtx, counted, netip.Addr{}, nil, nil,
				)
			}()
			if err := pair.client.Send(context.Background(), &bin.Buffer{Buf: shutdownClientFrame(t, key, 42, 1<<32, &tg.HelpGetConfigRequest{})}); err != nil {
				t.Fatalf("send active RPC: %v", err)
			}
			var contexts rpcContexts
			select {
			case contexts = <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("RPC handler did not start")
			}

			server.shutdown.beginDrain()
			select {
			case <-contexts.requestDone:
				t.Fatal("drain start canceled the admitted RPC")
			default:
			}
			select {
			case <-contexts.completionDone:
				t.Fatal("drain start canceled the post-commit completion context")
			default:
			}
			select {
			case err := <-serveDone:
				t.Fatalf("serve returned before admitted RPC finished: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if servedConn.closeSource.Load() != closeSourceUnknown || servedConn.peerLost.Load() {
				t.Fatalf("drain-start close provenance = %d, peer lost %t", servedConn.closeSource.Load(), servedConn.peerLost.Load())
			}

			releaseHandler()
			assertPeerBoolResult(t, pair.client, key, 1<<32)
			select {
			case err := <-serveDone:
				if err != nil {
					t.Fatalf("serve after drain completion: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("serve loop did not return after drain completion")
			}
			if counted.active.Load() != 0 || counted.maximum.Load() != 1 {
				t.Fatalf("transport Recv state after serve: active %d, maximum concurrent %d", counted.active.Load(), counted.maximum.Load())
			}
			if _, denial := server.peerCancels.reserve(7); denial != peerCancelAllowed {
				t.Fatalf("normal drain completion consumed peer allowance: next reservation = %q", denial)
			}
		})
	}
}

func TestLongRPCOutlivesFrameReadTimeoutOnTCPAndWebSocket(t *testing.T) {
	for _, websocketTransport := range []bool{false, true} {
		name := "tcp"
		if websocketTransport {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			key := shutdownTestAuthKey()
			entered := make(chan struct{}, 1)
			var servedConn *Conn
			server := New(exchange.PrivateKey{}, 2, &shutdownLookupAuthKeyStore{key: key}, HandlerFunc(func(c *Conn, req *Request) error {
				servedConn = c
				entered <- struct{}{}
				timer := time.NewTimer(180 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-req.Ctx.Done():
					return req.Ctx.Err()
				case <-timer.C:
					return c.SendResult(req, &tg.BoolTrue{})
				}
			}), nil)
			server.readTimeout = 80 * time.Millisecond
			if err := server.SetRPCDeadline(45 * time.Second); err != nil {
				t.Fatalf("set maximum RPC deadline: %v", err)
			}
			pair := openShutdownTransportPair(t, server, websocketTransport, nil)
			t.Cleanup(pair.close)
			t.Cleanup(func() { stopShutdownTimers(server.shutdown) })
			counted := &peerCountingTransport{Conn: pair.server}
			serveDone := make(chan error, 1)
			go func() {
				serveDone <- server.serveConnWithContexts(
					context.Background(), server.shutdown.requestCtx, counted, netip.Addr{}, nil, nil,
				)
			}()
			if err := pair.client.Send(context.Background(), &bin.Buffer{Buf: shutdownClientFrame(t, key, 42, 1<<32, &tg.HelpGetConfigRequest{})}); err != nil {
				t.Fatalf("send long RPC: %v", err)
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("long RPC handler did not start")
			}
			assertPeerBoolResult(t, pair.client, key, 1<<32)
			select {
			case err := <-serveDone:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("serve after idle frame timeout = %v, want context.DeadlineExceeded", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("idle frame timeout did not retire the connection")
			}
			if counted.active.Load() != 0 || counted.maximum.Load() != 1 {
				t.Fatalf("transport Recv state after long RPC: active %d, maximum concurrent %d", counted.active.Load(), counted.maximum.Load())
			}
			if servedConn == nil || servedConn.closeSource.Load() != closeSourceServer || servedConn.peerLost.Load() {
				t.Fatalf("idle read timeout provenance = %v, want server-owned close", servedConn)
			}
			if _, denial := server.peerCancels.reserve(7); denial != peerCancelAllowed {
				t.Fatalf("frame read timeout consumed peer allowance: next reservation = %q", denial)
			}
		})
	}
}

func TestPeerReaderBuffersOneFrameWithBackpressure(t *testing.T) {
	key := shutdownTestAuthKey()
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	var calls atomic.Int32
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save peer reader key: %v", err)
	}
	server := New(exchange.PrivateKey{}, 2, keys, HandlerFunc(func(c *Conn, req *Request) error {
		switch calls.Add(1) {
		case 1:
			firstStarted <- struct{}{}
			<-releaseFirst
		case 2:
			secondStarted <- struct{}{}
		}
		return c.SendResult(req, &tg.BoolTrue{})
	}), nil)
	server.readTimeout = time.Second
	pair := openShutdownTransportPair(t, server, false, nil)
	t.Cleanup(pair.close)
	t.Cleanup(releaseHandler)
	counted := &peerCountingTransport{Conn: pair.server}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.serveConnWithContexts(
			context.Background(), server.shutdown.requestCtx, counted, netip.Addr{}, nil, nil,
		)
	}()

	for i := int64(1); i <= 3; i++ {
		if err := pair.client.Send(context.Background(), &bin.Buffer{Buf: shutdownClientFrame(t, key, 42, i<<32, &tg.HelpGetConfigRequest{})}); err != nil {
			t.Fatalf("send RPC frame %d: %v", i, err)
		}
		if i == 1 {
			select {
			case <-firstStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("first RPC handler did not start")
			}
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for counted.receives.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := counted.receives.Load(); got != 2 {
		t.Fatalf("transport receives while first RPC is active = %d, want request plus one prefetch", got)
	}
	select {
	case <-secondStarted:
		t.Fatal("second RPC started before the active RPC finished")
	default:
	}
	select {
	case err := <-serveDone:
		t.Fatalf("serve loop returned while first RPC was blocked: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if got := counted.receives.Load(); got != 2 {
		t.Fatalf("peer reader read past its one-frame buffer: %d receives", got)
	}

	releaseHandler()
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("buffered second RPC did not start after the first finished")
	}
	deadline = time.Now().Add(2 * time.Second)
	for counted.receives.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if counted.receives.Load() < 3 || counted.maximum.Load() != 1 {
		t.Fatalf("peer reader state after ordered dispatch: receives %d, max concurrent %d", counted.receives.Load(), counted.maximum.Load())
	}
	if err := pair.client.Close(); err != nil && !isDisconnect(err) {
		t.Fatalf("close client transport: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil && !isDisconnect(err) && !errors.Is(err, context.Canceled) {
			t.Fatalf("serve after peer close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("peer reader did not exit after peer close")
	}
	if counted.active.Load() != 0 {
		t.Fatalf("transport Recv remained active after serve returned: %d", counted.active.Load())
	}
}

func assertPeerBoolResult(t *testing.T, conn transport.Conn, key crypto.AuthKey, msgID int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for {
		var frame bin.Buffer
		if err := conn.Recv(ctx, &frame); err != nil {
			t.Fatalf("receive admitted RPC result: %v", err)
		}
		encrypted := &crypto.EncryptedMessage{}
		if err := encrypted.DecodeWithoutCopy(&frame); err != nil {
			continue
		}
		message, err := cipher.Decrypt(key, encrypted)
		if err != nil {
			t.Fatalf("decrypt admitted RPC result: %v", err)
		}
		var result proto.Result
		if err := result.Decode(&bin.Buffer{Buf: message.Data()}); err != nil || result.RequestMessageID != msgID {
			continue
		}
		answer := &tg.BoolTrue{}
		if err := answer.Decode(&bin.Buffer{Buf: result.Result}); err != nil {
			t.Fatalf("decode admitted RPC result: %v", err)
		}
		return
	}
}

func errorsIsCanceled(ctx context.Context) bool {
	return ctx.Err() == context.Canceled
}
