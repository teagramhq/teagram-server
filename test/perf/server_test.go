//go:build performance

package perf_test

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

const dcID = 2

// contactCap admits the fixture's 10,000 owner contacts; the shipped
// per-account bound is 5,000.
const contactCap = 10_000

// channelParticipantCap admits the fixture's 100,000-subscriber channel; the
// shipped per-channel bound is 10,000.
const channelParticipantCap = 200_000

// server is one in-process teagramd: the shipped handler, updater and LISTEN
// fan-out, over a store whose every query passes through tracer.
type server struct {
	store    *store.Store
	tracer   *queryTracer
	rpcTimer *rpcTimingHandler
	registry *mtproto.SessionRegistry
	key      *rsa.PrivateKey
	port     int
	stop     func()
	// accepted receives the meter of every connection the server accepts.
	accepted chan *meter
}

// rpcTimingHandler times dispatched RPCs only while one perf scenario opens a
// timing window. It also tags the request context so SQL spans use this same
// boundary. A dispatched handler returns after Conn.SendResult has completed
// the full transport write. MTProto service messages such as msgs_ack are
// handled before this boundary and do not enter the timer.
type rpcTimingHandler struct {
	next     mtproto.Handler
	active   atomic.Bool
	mu       sync.Mutex
	enabled  bool
	inFlight int
	samples  []time.Duration
	done     chan struct{}
}

func (h *rpcTimingHandler) start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.enabled || h.inFlight != 0 {
		return errors.New("RPC timing window is already active")
	}
	h.samples = nil
	h.done = make(chan struct{})
	h.enabled = true
	h.active.Store(true)
	return nil
}

func (h *rpcTimingHandler) finish(ctx context.Context) (time.Duration, error) {
	h.active.Store(false)
	h.mu.Lock()
	if !h.enabled || h.done == nil {
		h.mu.Unlock()
		return 0, errors.New("RPC timing window is not active")
	}
	h.enabled = false
	done := h.done
	if h.inFlight == 0 {
		close(done)
		h.done = nil
	}
	h.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) != 1 {
		return 0, fmt.Errorf("timed RPC dispatches = %d, want exactly one", len(h.samples))
	}
	return h.samples[0], nil
}

func (h *rpcTimingHandler) OnMessage(c *mtproto.Conn, req *mtproto.Request) error {
	if !h.active.Load() || !isContactsGetContacts(req) {
		return h.next.OnMessage(c, req)
	}
	h.mu.Lock()
	if !h.enabled {
		h.mu.Unlock()
		return h.next.OnMessage(c, req)
	}
	h.inFlight++
	h.mu.Unlock()

	started := time.Now()
	window := newQueryTimingWindow(started)
	requestCtx := req.Ctx
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	req.Ctx = context.WithValue(requestCtx, queryTimingWindowContextKey{}, window)
	err := h.next.OnMessage(c, req)
	finished := time.Now()
	window.finish(finished)
	elapsed := finished.Sub(started)

	h.mu.Lock()
	h.samples = append(h.samples, elapsed)
	h.inFlight--
	if !h.enabled && h.inFlight == 0 {
		close(h.done)
		h.done = nil
	}
	h.mu.Unlock()
	return err
}

func isContactsGetContacts(req *mtproto.Request) bool {
	if req == nil || req.Buf == nil {
		return false
	}
	b := &bin.Buffer{Buf: req.Buf.Copy()}
	id, err := b.PeekID()
	if err != nil {
		return false
	}
	for {
		query := &rpcConstructorID{}
		var wrapper interface{ Decode(*bin.Buffer) error }
		switch id {
		case tg.InvokeWithLayerRequestTypeID:
			wrapper = &tg.InvokeWithLayerRequest{Query: query}
		case tg.InitConnectionRequestTypeID:
			wrapper = &tg.InitConnectionRequest{Query: query}
		case tg.InvokeWithoutUpdatesRequestTypeID:
			wrapper = &tg.InvokeWithoutUpdatesRequest{Query: query}
		case tg.InvokeAfterMsgRequestTypeID:
			wrapper = &tg.InvokeAfterMsgRequest{Query: query}
		default:
			return id == tg.ContactsGetContactsRequestTypeID
		}
		if err := wrapper.Decode(b); err != nil {
			return false
		}
		id = query.id
	}
}

type rpcConstructorID struct {
	id uint32
}

func (p *rpcConstructorID) Decode(b *bin.Buffer) error {
	id, err := b.PeekID()
	if err == nil {
		p.id = id
	}
	return err
}

func (*rpcConstructorID) Encode(*bin.Buffer) error {
	return errors.New("RPC constructor ID peeker is decode-only")
}

func TestRPCTimingHandlerWaitsForDispatchCompletion(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	h := &rpcTimingHandler{next: mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
		close(started)
		<-release
		return nil
	})}
	if err := h.start(); err != nil {
		t.Fatal(err)
	}

	dispatchDone := make(chan error, 1)
	request := perfRPCRequest(t, &tg.ContactsGetContactsRequest{})
	go func() { dispatchDone <- h.OnMessage(nil, request) }()
	<-started
	type result struct {
		elapsed time.Duration
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		elapsed, err := h.finish(context.Background())
		finished <- result{elapsed: elapsed, err: err}
	}()
	select {
	case got := <-finished:
		t.Fatalf("finish returned while the reply write was blocked: %+v", got)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-dispatchDone; err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got := <-finished
	if got.err != nil {
		t.Fatalf("finish: %v", got.err)
	}
	if got.elapsed < 10*time.Millisecond {
		t.Fatalf("elapsed = %s, want at least the blocked reply duration", got.elapsed)
	}
}

func TestRPCTimingHandlerCorrelatesContactsRPC(t *testing.T) {
	calls := 0
	h := &rpcTimingHandler{next: mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
		calls++
		return nil
	})}
	if err := h.start(); err != nil {
		t.Fatal(err)
	}
	if err := h.OnMessage(nil, perfRPCRequest(t, &tg.HelpGetConfigRequest{})); err != nil {
		t.Fatalf("dispatch unrelated RPC: %v", err)
	}
	contacts := &tg.InvokeWithLayerRequest{Layer: 100, Query: &tg.InitConnectionRequest{
		Query: &tg.InvokeWithoutUpdatesRequest{Query: &tg.ContactsGetContactsRequest{}},
	}}
	if err := h.OnMessage(nil, perfRPCRequest(t, contacts)); err != nil {
		t.Fatalf("dispatch contacts RPC: %v", err)
	}
	if _, err := h.finish(context.Background()); err != nil {
		t.Fatalf("finish contacts timing: %v", err)
	}
	if calls != 2 {
		t.Fatalf("dispatches = %d, want both RPCs forwarded", calls)
	}
}

func TestRPCTimingHandlerScopesSQLToContactsDispatch(t *testing.T) {
	tracer := &queryTracer{}
	tracer.reset(false, true)
	trace := func(ctx context.Context, name string) {
		t.Helper()
		ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "-- name: " + name + " :one\nSELECT 1"})
		tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	}
	h := &rpcTimingHandler{next: mtproto.HandlerFunc(func(_ *mtproto.Conn, req *mtproto.Request) error {
		trace(req.Ctx, "ListContacts")
		return nil
	})}
	if err := h.start(); err != nil {
		t.Fatal(err)
	}
	trace(context.Background(), "AuthKeyByID")
	request := perfRPCRequest(t, &tg.ContactsGetContactsRequest{})
	request.Ctx = context.Background()
	if err := h.OnMessage(nil, request); err != nil {
		t.Fatalf("dispatch contacts RPC: %v", err)
	}
	trace(context.Background(), "AfterReply")
	if _, err := h.finish(context.Background()); err != nil {
		t.Fatalf("finish contacts timing: %v", err)
	}

	got := tracer.timingWindow()
	if len(got) != 1 || got[0].name != "ListContacts" {
		t.Fatalf("SQL timings = %+v, want only ListContacts inside the RPC dispatch window", got)
	}
}

func perfRPCRequest(t *testing.T, req bin.Encoder) *mtproto.Request {
	t.Helper()
	var body bin.Buffer
	if err := req.Encode(&body); err != nil {
		t.Fatalf("encode test RPC: %v", err)
	}
	return &mtproto.Request{Buf: &body}
}

func bootServer(t *testing.T, ctx context.Context, dsn, blobDir string, key *rsa.PrivateKey) *server {
	t.Helper()
	tracer := &queryTracer{}
	blobs, err := blob.NewLocal(blobDir)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	st, err := store.Open(ctx, dsn, pgtest.EncKey(),
		store.WithBlobStore(blobs),
		store.WithQueryTracer(tracer),
		store.WithMaxContactsPerUser(contactCap),
		store.WithMaxChannelParticipants(channelParticipantCap),
		store.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr type = %T", ln.Addr())
	}
	log := serverLog()
	cfg := api.DefaultConfig(dcID, addr.IP.String(), addr.Port)
	sync := api.NewDialogFilterSync()
	handler := api.NewWithDialogFilterSync(st, dcID, cfg, log, false, 100<<20, blobs, 2<<30,
		pgtest.PeerDeriver(), pgtest.PhotoDeriver(), unboundRateLimits(), config.RegistrationClosed, sync)
	rpcTimer := &rpcTimingHandler{next: handler}
	srv := mtproto.New(exchange.PrivateKey{RSA: key}, dcID, mtproto.NewPgAuthKeyStore(st), rpcTimer, log)
	// Every exchange still passes the shipped budget checks; the budget itself
	// is raised past the samples the harness takes from one address.
	exchangeLimits := mtproto.DefaultExchangeLimits()
	exchangeLimits.PerNetwork.Limit = 1 << 30
	exchangeLimits.TailnetPerNetwork.Limit = 1 << 30
	exchangeLimits.GlobalRate, exchangeLimits.GlobalBurst = 1<<20, 1<<20
	if err := srv.SetExchangeLimits(exchangeLimits); err != nil {
		t.Fatalf("exchange limits: %v", err)
	}
	updater := api.NewUpdaterWithDialogFilterSync(st, dcID, srv.Registry(), log, pgtest.PeerDeriver(), sync)
	_, stopListener, err := store.StartListenerWithDialogPinsAndStickerSets(ctx, dsn,
		updater.Deliver, updater.DeliverTyping, updater.Evict, updater.DeliverChannelPost,
		updater.DeliverEncryption, updater.DeliverStatus, updater.DeliverEncryptedMsg,
		updater.DeliverReactions, updater.DeliverPinned, updater.MarkDialogFilters,
		updater.DeliverDialogPins, updater.DeliverStickerSets, updater.DialogFilterListenerReconnected, log)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	stopRecovery := updater.StartDialogFilterRecovery(ctx)
	metered := &meteredListener{Listener: ln, accepted: make(chan *meter, 64)}
	srvCtx, cancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(srvCtx, metered) }()
	s := &server{store: st, tracer: tracer, rpcTimer: rpcTimer, registry: srv.Registry(), key: key, port: addr.Port, accepted: metered.accepted}
	s.stop = func() {
		cancel()
		if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("serve: %v", err)
		}
		if err := stopListener(); err != nil {
			t.Errorf("stop listener: %v", err)
		}
		stopRecovery()
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}
	return s
}

// unboundRateLimits keeps every shipped limiter switched on, so each RPC pays
// its real limiter queries, but raises its budget past anything the harness
// sends.
func unboundRateLimits() config.RateLimitsConfig {
	limits := config.DefaultRateLimits()
	raise(reflect.ValueOf(&limits).Elem())
	return limits
}

func raise(v reflect.Value) {
	if v.Type() == reflect.TypeFor[store.RateLimitConfig]() {
		if cfg, ok := reflect.TypeAssert[*store.RateLimitConfig](v.Addr()); ok && cfg.Limit > 0 {
			cfg.Limit = 1 << 30
		}
		return
	}
	if v.Kind() == reflect.Struct {
		for _, field := range v.Fields() {
			raise(field)
		}
	}
}

// client is a connected gotd client. api is usable from any goroutine until
// close returns.
type client struct {
	api     *tg.Client
	updates chan tg.UpdatesClass
	close   func()
}

func (s *server) newClient(handler telegram.UpdateHandler, storage telegram.SessionStorage) *telegram.Client {
	return telegram.NewClient(1, "hash", telegram.Options{
		DC:             dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: dcID, IPAddress: "127.0.0.1", Port: s.port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &s.key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		UpdateHandler:  handler,
		SessionStorage: storage,
		NoUpdates:      handler == nil,
	})
}

// login connects a fresh auth key and binds it to userID, the way a session
// looks once sign-in has finished.
func (s *server) login(t *testing.T, ctx context.Context, userID int64) *client {
	t.Helper()
	c := &client{updates: make(chan tg.UpdatesClass, 1024)}
	storage := &session.StorageMemory{}
	tc := s.newClient(telegram.UpdateHandlerFunc(func(_ context.Context, u tg.UpdatesClass) error {
		select {
		case c.updates <- u:
		default:
		}
		return nil
	}), storage)
	ready := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- tc.Run(runCtx, func(ctx context.Context) error {
			if _, err := tc.API().HelpGetConfig(ctx); err != nil {
				ready <- fmt.Errorf("initialize session: %w", err)
				return err
			}
			data, err := (&session.Loader{Storage: storage}).Load(ctx)
			if err != nil {
				ready <- fmt.Errorf("load auth key: %w", err)
				return err
			}
			var id [8]byte
			copy(id[:], data.AuthKeyID)
			if err := s.store.BindAuthKeyUser(ctx, mtproto.AuthKeyIDInt64(id), userID); err != nil {
				ready <- fmt.Errorf("bind auth key: %w", err)
				return err
			}
			if _, err := tc.Self(ctx); err != nil {
				ready <- fmt.Errorf("self: %w", err)
				return err
			}
			// A client's first call after sign-in; it moves the session's update
			// cursor to now, so deliveries carry only new events.
			if _, err := tc.API().UpdatesGetState(ctx); err != nil {
				ready <- fmt.Errorf("get state: %w", err)
				return err
			}
			ready <- nil
			<-ctx.Done()
			return nil
		})
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			t.Fatalf("login %d: %v", userID, err)
		}
	case <-time.After(60 * time.Second):
		cancel()
		t.Fatalf("login %d: timeout", userID)
	}
	c.api = tc.API()
	c.close = func() {
		cancel()
		<-done
	}
	return c
}

// exchangeKey runs one complete auth key exchange on a new connection and the
// first RPC on the new key, and returns the time the server spent answering.
// The client's own share is left out: gotd checks the DH prime's primality on
// every exchange, about two seconds that no real client spends twice.
func (s *server) exchangeKey(ctx context.Context) (time.Duration, error) {
	for len(s.accepted) > 0 {
		<-s.accepted
	}
	tc := s.newClient(nil, &session.StorageMemory{})
	if err := tc.Run(ctx, func(ctx context.Context) error {
		_, err := tc.API().HelpGetConfig(ctx)
		return err
	}); err != nil {
		return 0, err
	}
	select {
	case m := <-s.accepted:
		return m.serverTime(), nil
	default:
		return 0, errors.New("exchange connection was not accepted")
	}
}

// meteredListener hands each accepted connection's meter to the harness.
type meteredListener struct {
	net.Listener

	accepted chan *meter
}

func (l *meteredListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	m := &meter{}
	select {
	case l.accepted <- m:
	default:
	}
	return &meteredConn{Conn: c, m: m}, nil
}

// meter sums, over a connection's life, the time from the first byte of each
// client request to the server's reply.
type meter struct {
	mu      sync.Mutex
	busy    time.Duration
	readAt  time.Time
	waiting bool
}

func (m *meter) serverTime() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy
}

type meteredConn struct {
	net.Conn

	m *meter
}

func (c *meteredConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.m.mu.Lock()
		if !c.m.waiting {
			c.m.readAt, c.m.waiting = time.Now(), true
		}
		c.m.mu.Unlock()
	}
	return n, err
}

func (c *meteredConn) Write(b []byte) (int, error) {
	c.m.mu.Lock()
	if c.m.waiting {
		c.m.busy += time.Since(c.m.readAt)
		c.m.waiting = false
	}
	c.m.mu.Unlock()
	return c.Conn.Write(b)
}

// serverLog discards the server's log unless TG_PERF_LOG is set.
func serverLog() *slog.Logger {
	if os.Getenv("TG_PERF_LOG") == "" {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestFreshSessionPushesOnlyUpdatesAfterGetState(t *testing.T) {
	dir := perfDir(t)
	spec := fixtures[0]
	dsn, ok := fixtureDSN(spec)
	if !ok {
		t.Skip("TG_PERF_DSN_1X is not set")
	}
	f, err := loadFixture(fixturePath(dir, spec))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	key, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "key.pem"))
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	srv := bootServer(t, ctx, dsn, blobDir(dir, spec), key)
	client := srv.login(t, ctx, f.Owner)
	defer client.close()
	sender := srv.login(t, ctx, f.Partner)
	defer sender.close()
	conns := srv.registry.Conns(f.Owner)
	if len(conns) != 1 {
		t.Fatalf("registered connections = %d, want one", len(conns))
	}
	state, err := srv.store.StateWithoutUnread(ctx, f.Owner)
	if err != nil {
		t.Fatalf("read owner state: %v", err)
	}
	if got := conns[0].LastPushedPts(); got != state.Pts {
		t.Fatalf("watermark after getState = %d, want state pts %d", got, state.Pts)
	}
	if err := srv.tracer.awaitQuiet(ctx, 10*time.Second); err != nil {
		t.Fatalf("server did not go quiet after getState: %v", err)
	}
	drain(client)
	drain(sender)

	const messageText = "only the new update"
	if _, err := sender.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer: userPeer(f.Partner, f.Owner), Message: messageText, RandomID: randomIDs.Add(1),
	}); err != nil {
		t.Fatalf("send post-state message: %v", err)
	}
	var got tg.UpdatesClass
	select {
	case got = <-client.updates:
	case <-time.After(10 * time.Second):
		t.Fatal("fresh session did not receive the post-state message")
	case <-ctx.Done():
		t.Fatalf("wait for post-state update: %v", ctx.Err())
	}
	if err := srv.tracer.awaitQuiet(ctx, 10*time.Second); err != nil {
		t.Fatalf("server did not go quiet after delivery: %v", err)
	}
	updates, ok := got.(*tg.Updates)
	if !ok {
		t.Fatalf("push = %T, want *tg.Updates", got)
	}
	if len(updates.Updates) != 1 {
		t.Fatalf("push contains %d updates, want exactly one new update", len(updates.Updates))
	}
	newMessage, ok := updates.Updates[0].(*tg.UpdateNewMessage)
	if !ok {
		t.Fatalf("update = %T, want *tg.UpdateNewMessage", updates.Updates[0])
	}
	message, ok := newMessage.Message.(*tg.Message)
	if !ok || message.Message != messageText {
		t.Fatalf("pushed message = %#v, want only %q", newMessage.Message, messageText)
	}
	select {
	case extra := <-client.updates:
		t.Fatalf("fresh session received an extra update: %T", extra)
	default:
	}
}
