package mtproto_test

import (
	"context"
	crand "crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

type mixedExchangeKeyStore struct {
	mu        sync.Mutex
	key       crypto.AuthKey
	userID    int64
	present   bool
	lookupErr error

	gets    int
	saves   []crypto.AuthKey
	touches int
}

type mixedExchangeKeyStoreState struct {
	userID  int64
	present bool
	saves   int
	touches int
}

func (s *mixedExchangeKeyStore) Save(_ context.Context, key crypto.AuthKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, key)
	s.key = key
	s.present = true
	return nil
}

func (s *mixedExchangeKeyStore) Get(_ context.Context, id [8]byte, _ time.Duration) (crypto.AuthKey, int64, bool, mtproto.PendingLogin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.lookupErr != nil {
		return crypto.AuthKey{}, 0, false, mtproto.PendingLogin{}, false, s.lookupErr
	}
	if s.present && id == s.key.ID {
		return s.key, s.userID, false, mtproto.PendingLogin{}, true, nil
	}
	return crypto.AuthKey{}, 0, false, mtproto.PendingLogin{}, false, nil
}

func (s *mixedExchangeKeyStore) Touch(context.Context, [8]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touches++
	return nil
}

func (*mixedExchangeKeyStore) CheckIPRateLimit(context.Context, netip.Addr, string, store.RateLimitConfig) (*store.RateLimitResult, error) {
	return nil, nil //nolint:nilnil // test key stores do not keep a shared network counter.
}

func (s *mixedExchangeKeyStore) snapshot() mixedExchangeKeyStoreState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return mixedExchangeKeyStoreState{
		userID:  s.userID,
		present: s.present,
		saves:   len(s.saves),
		touches: s.touches,
	}
}

func (s *mixedExchangeKeyStore) setPresent(present bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.present = present
}

func (s *mixedExchangeKeyStore) setBinding(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userID = userID
}

type mixedExchangeRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	client transport.Conn
	server *mtproto.Server
	done   chan error
	exited chan struct{}
}

func startMixedExchangeServer(t *testing.T, keys mtproto.AuthKeyStore, handler mtproto.Handler) *mixedExchangeRun {
	t.Helper()
	return startMixedExchangeConn(t, newMixedExchangeServer(t, keys, handler))
}

func newMixedExchangeServer(t *testing.T, keys mtproto.AuthKeyStore, handler mtproto.Handler) *mtproto.Server {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(crand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return mtproto.New(exchange.PrivateKey{RSA: rsaKey}, 2, keys, handler, nil)
}

func startMixedExchangeConn(t *testing.T, server *mtproto.Server) *mixedExchangeRun {
	t.Helper()

	client, serverConn := transport.Intermediate.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	run := &mixedExchangeRun{
		ctx:    ctx,
		cancel: cancel,
		client: client,
		server: server,
		done:   make(chan error, 1),
		exited: make(chan struct{}),
	}
	go func() {
		run.done <- server.ServeConn(ctx, serverConn)
		close(run.exited)
	}()
	t.Cleanup(func() {
		cancel()
		if err := client.Close(); err != nil {
			t.Errorf("close client transport: %v", err)
		}
		select {
		case <-run.exited:
		case <-time.After(2 * time.Second):
			t.Error("server connection did not exit")
		}
	})
	return run
}

func beginMixedExchange(t *testing.T, run *mixedExchangeRun) {
	t.Helper()
	beginKeyExchange(t, run.ctx, run.client)
}

func TestKeyExchangeCountSurvivesPreAuthSlotClear(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	rsaKey, err := rsa.GenerateKey(crand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	privateKey := exchange.PrivateKey{RSA: rsaKey}
	keys := &mixedExchangeKeyStore{}
	handler := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		return c.SendResult(req, &tg.BoolTrue{})
	})
	server := mtproto.New(privateKey, 2, keys, handler, nil)
	listener := mustListenTCP(t, ctx, "127.0.0.1:0")
	serverCtx, stopServer := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.Serve(serverCtx, listener) }()
	t.Cleanup(func() {
		stopServer()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})

	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	client, err := transport.Intermediate.Handshake(rawConn)
	if err != nil {
		t.Fatalf("handshake transport: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})

	for exchangeNumber := 1; exchangeNumber <= 2; exchangeNumber++ {
		result, err := exchange.NewExchanger(client, 2).
			Client([]exchange.PublicKey{privateKey.Public()}).Run(ctx)
		if err != nil {
			t.Fatalf("complete exchange %d: %v", exchangeNumber, err)
		}
		requestID := int64(proto.NewMessageID(time.Now(), proto.MessageFromClient))
		frame := clientFrame(t, result.AuthKey, 72, requestID, &tg.HelpGetConfigRequest{})
		if err := client.Send(ctx, &bin.Buffer{Buf: frame}); err != nil {
			t.Fatalf("send MAC-valid frame after exchange %d: %v", exchangeNumber, err)
		}
		waitForMixedExchangeRPCResult(t, ctx, client, result.AuthKey, requestID)
	}

	request := mt.ReqPqMultiRequest{Nonce: bin.Int128{1}}
	var requestBody bin.Buffer
	if err := request.Encode(&requestBody); err != nil {
		t.Fatalf("encode req_pq_multi: %v", err)
	}
	var first bin.Buffer
	if err := (proto.UnencryptedMessage{
		MessageID:   int64(proto.NewMessageID(time.Now(), proto.MessageFromClient)),
		MessageData: requestBody.Copy(),
	}).Encode(&first); err != nil {
		t.Fatalf("encode third exchange frame: %v", err)
	}
	if err := client.Send(ctx, &first); err != nil {
		t.Fatalf("send third exchange: %v", err)
	}
	var refusal bin.Buffer
	err = client.Recv(ctx, &refusal)
	protocolErr, ok := errors.AsType[*codec.ProtocolErr](err)
	if !ok || protocolErr.Code != 429 {
		t.Fatalf("third exchange refusal = %v, want protocol error -429", err)
	}
	if err := client.Recv(ctx, &refusal); err == nil {
		t.Fatal("connection remained open after the third exchange refusal")
	}
	state := keys.snapshot()
	if state.saves != 2 {
		t.Fatalf("saved auth keys = %d, want two successful exchanges", state.saves)
	}
}

func waitForMixedExchangeRPCResult(t *testing.T, ctx context.Context, client transport.Conn, key crypto.AuthKey, requestID int64) {
	t.Helper()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for {
		var wire bin.Buffer
		if err := client.Recv(ctx, &wire); err != nil {
			t.Fatalf("receive authenticated-frame response: %v", err)
		}
		message, err := cipher.DecryptFromBuffer(key, &wire)
		if err != nil {
			t.Fatalf("decrypt authenticated-frame response: %v", err)
		}
		body := &bin.Buffer{Buf: message.MessageDataWithPadding[:message.MessageDataLen]}
		id, err := body.PeekID()
		if err != nil {
			t.Fatalf("peek authenticated-frame response: %v", err)
		}
		if id == mt.NewSessionCreatedTypeID {
			continue
		}
		var result proto.Result
		if err := result.Decode(body); err != nil {
			continue
		}
		if result.RequestMessageID == requestID {
			return
		}
	}
}

func beginKeyExchange(t *testing.T, ctx context.Context, client transport.Conn) {
	t.Helper()
	request := mt.ReqPqMultiRequest{Nonce: bin.Int128{1}}
	var body bin.Buffer
	if err := request.Encode(&body); err != nil {
		t.Fatalf("encode req_pq_multi: %v", err)
	}
	message := proto.UnencryptedMessage{
		MessageID:   int64(proto.NewMessageID(time.Now(), proto.MessageFromClient)),
		MessageData: body.Copy(),
	}
	var frame bin.Buffer
	if err := message.Encode(&frame); err != nil {
		t.Fatalf("encode unencrypted exchange frame: %v", err)
	}
	if err := client.Send(ctx, &frame); err != nil {
		t.Fatalf("send req_pq_multi: %v", err)
	}
	var response bin.Buffer
	if err := client.Recv(ctx, &response); err != nil {
		t.Fatalf("receive res_pq: %v", err)
	}
	var unencrypted proto.UnencryptedMessage
	if err := unencrypted.Decode(&response); err != nil {
		t.Fatalf("decode res_pq: %v", err)
	}
	var resPQ mt.ResPQ
	if err := resPQ.Decode(&bin.Buffer{Buf: unencrypted.MessageData}); err != nil {
		t.Fatalf("decode res_pq body: %v", err)
	}
}

func TestServeConnProcessesStoredKeyFrameDuringExchange(t *testing.T) {
	key := rebindTestKey()
	keys := &mixedExchangeKeyStore{key: key, userID: 7, present: true}
	requests := make(chan *mtproto.Request, 1)
	run := startMixedExchangeServer(t, keys, mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		requests <- req
		return nil
	}))
	beginMixedExchange(t, run)

	frame := clientFrame(t, key, 52, int64(1)<<32, &tg.HelpGetConfigRequest{})
	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send stored-key request during exchange: %v", err)
	}
	// The first authenticated frame sends new_session_created before dispatch.
	// Receiving it also unblocks that write on the in-memory transport pipe.
	var response bin.Buffer
	if err := run.client.Recv(run.ctx, &response); err != nil {
		t.Fatalf("receive authenticated-frame response: %v", err)
	}
	select {
	case req := <-requests:
		if req.UserID != 7 || req.AuthKeyID != key.ID {
			t.Fatalf("request identity = user %d, key %x; want user 7, key %x", req.UserID, req.AuthKeyID, key.ID)
		}
	case <-run.ctx.Done():
		t.Fatal("stored-key frame did not reach the request handler")
	}

	if err := run.client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	<-run.done
	state := keys.snapshot()
	if state.saves != 0 {
		t.Fatalf("saved auth keys = %d, want no exchange key saved", state.saves)
	}
	if !state.present || state.userID != 7 {
		t.Fatalf("stored key state = present %t, user %d; want present and still bound to user 7", state.present, state.userID)
	}
	if state.touches != 1 {
		t.Fatalf("last-seen touches = %d, want one authenticated request touch", state.touches)
	}
}

func TestMixedExchangeFrameKeepsRevocationCheckOnNextFrame(t *testing.T) {
	key := rebindTestKey()
	keys := &mixedExchangeKeyStore{key: key, userID: 7, present: true}
	requests := make(chan *mtproto.Request, 2)
	run := startMixedExchangeServer(t, keys, mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		requests <- req
		return nil
	}))
	beginMixedExchange(t, run)
	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: clientFrame(t, key, 59, int64(1)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send mixed frame: %v", err)
	}
	var response bin.Buffer
	if err := run.client.Recv(run.ctx, &response); err != nil {
		t.Fatalf("receive mixed-frame response: %v", err)
	}
	select {
	case <-requests:
	case <-run.ctx.Done():
		t.Fatal("mixed frame did not reach the handler")
	}

	keys.setPresent(false)
	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: clientFrame(t, key, 59, int64(2)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send frame after revocation: %v", err)
	}
	var revokedResponse bin.Buffer
	err := run.client.Recv(run.ctx, &revokedResponse)
	var protocolErr *codec.ProtocolErr
	if err == nil || errors.As(err, &protocolErr) {
		t.Fatalf("revoked-key response = %v, want connection close", err)
	}
	if serveErr := <-run.done; serveErr != nil {
		t.Fatalf("ServeConn after revocation = %v, want clean close", serveErr)
	}
	state := keys.snapshot()
	if len(requests) != 0 || state.touches != 1 || state.saves != 0 {
		t.Fatalf("revocation side effects: requests=%d touches=%d saves=%d, want no second request, touch, or exchange key", len(requests), state.touches, state.saves)
	}
	if got := run.server.Registry().TotalConns(); got != 0 {
		t.Fatalf("registered connections after revocation = %d, want 0", got)
	}
}

func TestRevokedRetainedKeyUnbindsSocketBeforeNextDispatch(t *testing.T) {
	key := rebindTestKey()
	keys := &mixedExchangeKeyStore{key: key, userID: 7, present: true}
	var server *mtproto.Server
	unboundRegistryCount := make(chan int, 1)
	handler := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		if req.UserID == 0 {
			unboundRegistryCount <- server.Registry().TotalConns()
			return c.SendErr(req, tgerr.New(401, "AUTH_KEY_UNREGISTERED"))
		}
		return c.SendResult(req, &tg.BoolTrue{})
	})
	server = newMixedExchangeServer(t, keys, handler)
	run := startMixedExchangeConn(t, server)
	beginMixedExchange(t, run)

	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: clientFrame(t, key, 63, int64(1)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send live-key frame: %v", err)
	}
	for range 2 {
		var response bin.Buffer
		if err := run.client.Recv(run.ctx, &response); err != nil {
			t.Fatalf("receive live-key response: %v", err)
		}
	}
	for range 200 {
		if server.Registry().TotalConns() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := server.Registry().TotalConns(); got != 1 {
		t.Fatalf("registered connections before revoke = %d, want 1", got)
	}

	keys.setBinding(0)
	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: clientFrame(t, key, 63, int64(2)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send frame after unbind: %v", err)
	}
	var response bin.Buffer
	if err := run.client.Recv(run.ctx, &response); err != nil {
		t.Fatalf("receive revoked-key response: %v", err)
	}
	var encrypted crypto.EncryptedMessage
	if err := encrypted.DecodeWithoutCopy(&response); err != nil {
		t.Fatalf("decode revoked-key reply: %v", err)
	}
	decrypted, err := crypto.NewClientCipher(crypto.DefaultRand()).Decrypt(key, &encrypted)
	if err != nil {
		t.Fatalf("decrypt revoked-key reply: %v", err)
	}
	var result proto.Result
	if err := result.Decode(&bin.Buffer{Buf: decrypted.Data()}); err != nil {
		t.Fatalf("decode revoked-key RPC result: %v", err)
	}
	var rpcErr mt.RPCError
	if err := rpcErr.Decode(&bin.Buffer{Buf: result.Result}); err != nil {
		t.Fatalf("decode revoked-key RPC error: %v", err)
	}
	if rpcErr.ErrorCode != 401 || rpcErr.ErrorMessage != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("revoked-key error = %d %q, want 401 AUTH_KEY_UNREGISTERED", rpcErr.ErrorCode, rpcErr.ErrorMessage)
	}
	select {
	case registered := <-unboundRegistryCount:
		if registered != 0 {
			t.Fatalf("registry held %d connection(s) while dispatching unbound key, want 0", registered)
		}
	case <-run.ctx.Done():
		t.Fatal("unbound frame did not reach the request handler")
	}
	if got := server.Registry().TotalConns(); got != 0 {
		t.Fatalf("registered connections after revoked frame = %d, want 0", got)
	}
}

func TestMixedExchangeFrameUsesUnboundKeyCap(t *testing.T) {
	key := rebindTestKey()
	keys := &mixedExchangeKeyStore{key: key, present: true}
	requests := make(chan *mtproto.Request, 2)
	handler := mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		requests <- req
		return nil
	})
	server := newMixedExchangeServer(t, keys, handler)
	if err := server.SetMaxConnsPerUnboundKey(1); err != nil {
		t.Fatalf("set unbound-key cap: %v", err)
	}
	first := startMixedExchangeConn(t, server)
	beginMixedExchange(t, first)
	if err := first.client.Send(first.ctx, &bin.Buffer{Buf: clientFrame(t, key, 56, int64(1)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send first mixed frame: %v", err)
	}
	var response bin.Buffer
	if err := first.client.Recv(first.ctx, &response); err != nil {
		t.Fatalf("receive first mixed-frame response: %v", err)
	}
	select {
	case req := <-requests:
		if req.UserID != 0 {
			t.Fatalf("first request user = %d, want unbound", req.UserID)
		}
	case <-first.ctx.Done():
		t.Fatal("first mixed frame did not reach the handler")
	}

	second := startMixedExchangeConn(t, first.server)
	beginMixedExchange(t, second)
	if err := second.client.Send(second.ctx, &bin.Buffer{Buf: clientFrame(t, key, 57, int64(2)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send second mixed frame: %v", err)
	}
	var refused bin.Buffer
	err := second.client.Recv(second.ctx, &refused)
	var protocolErr *codec.ProtocolErr
	if err == nil || errors.As(err, &protocolErr) {
		t.Fatalf("second mixed frame response = %v, want connection close at unbound-key cap", err)
	}
	if serveErr := <-second.done; serveErr != nil {
		t.Fatalf("second ServeConn = %v, want clean cap refusal", serveErr)
	}
	if len(requests) != 0 {
		t.Fatalf("handler received %d requests after cap was full, want zero", len(requests))
	}
	if state := keys.snapshot(); state.saves != 0 {
		t.Fatalf("saved auth keys = %d, want no exchange key saved", state.saves)
	}
	if err := first.client.Close(); err != nil {
		t.Fatalf("close first client: %v", err)
	}
	<-first.done
}

func TestMixedExchangeFrameUsesPerUserConnectionCap(t *testing.T) {
	key := rebindTestKey()
	keys := &mixedExchangeKeyStore{key: key, userID: 7, present: true}
	requests := make(chan *mtproto.Request, 1)
	run := startMixedExchangeServer(t, keys, mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		requests <- req
		return nil
	}))
	beginMixedExchange(t, run)
	for range mtproto.MaxUserConns {
		if !run.server.Registry().Add(7, &mtproto.Conn{}) {
			t.Fatal("could not fill per-user connection registry")
		}
	}

	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: clientFrame(t, key, 58, int64(1)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send mixed frame at user cap: %v", err)
	}
	var response bin.Buffer
	if err := run.client.Recv(run.ctx, &response); err != nil {
		t.Fatalf("receive mixed-frame response at user cap: %v", err)
	}
	select {
	case req := <-requests:
		if req.UserID != 7 {
			t.Fatalf("request user = %d, want 7", req.UserID)
		}
	case <-run.ctx.Done():
		t.Fatal("mixed frame did not reach the handler before the user cap closed the connection")
	}
	if serveErr := <-run.done; serveErr != nil {
		t.Fatalf("ServeConn at user cap = %v, want clean connection refusal", serveErr)
	}
	if got := len(run.server.Registry().Conns(7)); got != mtproto.MaxUserConns {
		t.Fatalf("user 7 connections after refusal = %d, want existing cap %d", got, mtproto.MaxUserConns)
	}
	if state := keys.snapshot(); state.touches != 0 || state.saves != 0 {
		t.Fatalf("user-cap side effects: touches=%d saves=%d, want no touch after refusal and no exchanged key", state.touches, state.saves)
	}
}

func TestServeConnRejectsInvalidMACForStoredKeyFrameDuringExchange(t *testing.T) {
	key := rebindTestKey()
	keys := &mixedExchangeKeyStore{key: key, userID: 7, present: true}
	requests := make(chan *mtproto.Request, 2)
	run := startMixedExchangeServer(t, keys, mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
		requests <- nil
		return nil
	}))
	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: clientFrame(t, key, 53, int64(1)<<32, &tg.HelpGetConfigRequest{})}); err != nil {
		t.Fatalf("send initial authenticated request: %v", err)
	}
	var initialResponse bin.Buffer
	if err := run.client.Recv(run.ctx, &initialResponse); err != nil {
		t.Fatalf("receive initial authenticated response: %v", err)
	}
	select {
	case <-requests:
	case <-run.ctx.Done():
		t.Fatal("initial authenticated request did not reach the handler")
	}
	beginMixedExchange(t, run)
	if got := run.server.Registry().TotalConns(); got != 0 {
		t.Fatalf("registered connections during exchange = %d, want old owner removed", got)
	}

	frame := clientFrame(t, key, 53, int64(2)<<32, &tg.HelpGetConfigRequest{})
	frame[8] ^= 0xff // Corrupt the message key while preserving the known auth key ID.
	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send invalid-MAC frame: %v", err)
	}
	var response bin.Buffer
	err := run.client.Recv(run.ctx, &response)
	var protocolErr *codec.ProtocolErr
	if errors.As(err, &protocolErr) && protocolErr.Code == codec.CodeAuthKeyNotFound {
		t.Fatal("invalid-MAC known key received -404 during exchange")
	}
	if err == nil {
		t.Fatal("invalid-MAC frame unexpectedly received a response")
	}
	serveErr := <-run.done
	if serveErr == nil || errors.Is(serveErr, io.EOF) {
		t.Fatalf("ServeConn error = %v, want a decryption failure", serveErr)
	}
	state := keys.snapshot()
	if len(requests) != 0 || state.touches != 1 || state.saves != 0 {
		t.Fatalf("invalid-MAC side effects: requests=%d touches=%d saves=%d, want no extra request or touch and no exchanged key", len(requests), state.touches, state.saves)
	}
	if got := run.server.Registry().TotalConns(); got != 0 {
		t.Fatalf("registered connections after invalid MAC = %d, want 0", got)
	}
	if !state.present || state.userID != 7 {
		t.Fatalf("stored key state = present %t, user %d; want unchanged user 7 binding", state.present, state.userID)
	}
}

func TestServeConnKeepsExchangeLookupMissesAt404(t *testing.T) {
	known := rebindTestKey()
	unknown := rebindTestKey()
	unknown.ID[0]++

	tests := []struct {
		name string
		key  crypto.AuthKey
		row  bool
	}{
		{name: "unknown key", key: unknown, row: true},
		{name: "revoked key", key: known, row: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keys := &mixedExchangeKeyStore{key: known, userID: 7, present: test.row}
			requests := 0
			run := startMixedExchangeServer(t, keys, mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
				requests++
				return nil
			}))
			beginMixedExchange(t, run)

			frame := clientFrame(t, test.key, 54, int64(1)<<32, &tg.HelpGetConfigRequest{})
			if err := run.client.Send(run.ctx, &bin.Buffer{Buf: frame}); err != nil {
				t.Fatalf("send unknown or revoked key frame: %v", err)
			}
			var response bin.Buffer
			err := run.client.Recv(run.ctx, &response)
			var protocolErr *codec.ProtocolErr
			if !errors.As(err, &protocolErr) || protocolErr.Code != codec.CodeAuthKeyNotFound {
				t.Fatalf("exchange miss response = %v, want -404", err)
			}
			if err := run.client.Close(); err != nil {
				t.Fatalf("close client: %v", err)
			}
			<-run.done
			state := keys.snapshot()
			if requests != 0 || state.saves != 0 {
				t.Fatalf("miss side effects: requests=%d saved keys=%d, want zero", requests, state.saves)
			}
			if test.row && !state.present {
				t.Fatal("unknown-key lookup changed the unrelated stored row")
			}
		})
	}
}

func TestServeConnClosesOnExchangeKeyLookupErrorWithout404(t *testing.T) {
	key := rebindTestKey()
	keys := &mixedExchangeKeyStore{key: key, userID: 7, present: true, lookupErr: errors.New("stored key decrypt error: SECRET-DETAIL")}
	requests := 0
	run := startMixedExchangeServer(t, keys, mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
		requests++
		return nil
	}))
	beginMixedExchange(t, run)

	frame := clientFrame(t, key, 55, int64(1)<<32, &tg.HelpGetConfigRequest{})
	if err := run.client.Send(run.ctx, &bin.Buffer{Buf: frame}); err != nil {
		t.Fatalf("send key frame: %v", err)
	}
	var response bin.Buffer
	err := run.client.Recv(run.ctx, &response)
	var protocolErr *codec.ProtocolErr
	if errors.As(err, &protocolErr) && protocolErr.Code == codec.CodeAuthKeyNotFound {
		t.Fatal("lookup error was represented as -404")
	}
	if err == nil {
		t.Fatal("lookup error did not close the connection")
	}
	if serveErr := <-run.done; serveErr == nil || errors.Is(serveErr, io.EOF) {
		t.Fatalf("ServeConn error = %v, want lookup failure", serveErr)
	}
	if state := keys.snapshot(); requests != 0 || state.saves != 0 || state.touches != 0 {
		t.Fatalf("lookup-error side effects: requests=%d saves=%d touches=%d, want zero", requests, state.saves, state.touches)
	}
}
