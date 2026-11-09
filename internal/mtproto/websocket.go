package mtproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/discovery"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	// websocketPath is the endpoint used by Telegram Web's MTProto transport.
	websocketPath = "/apiws"
	// clientPolicyHeader distinguishes browser-only refusals from other failed
	// WebSocket handshakes.
	clientPolicyHeader = "Teagram-Client-Policy"
	// cfConnectingIPHeader is trusted only on the separately bound public
	// WebSocket listener, whose host port is published on loopback for the
	// Cloudflare Tunnel connector.
	cfConnectingIPHeader = "CF-Connecting-IP"
	// clientDiscoveryPath publishes the public identity for browser clients.
	clientDiscoveryPath = "/.well-known/teagramd/client"
	// The MTProto codecs reject payloads larger than 16 MiB. The extra bytes
	// cover the largest transport and obfuscation headers that can share a
	// WebSocket message with one payload.
	maxWebSocketMessageSize = 16<<20 + 64
)

type webSocketConnKey struct{}

type webSocketConnState struct {
	socket   *webSocketAcceptedConn
	slot     *preAuthSlot
	addr     netip.Addr
	deadline time.Time
}

var errWebSocketTransportReadEOF = errors.New("WebSocket transport read returned EOF")

// markWebSocketTransportReadEOF distinguishes peer-read EOFs from server
// errors in stores or RPC handlers that happen to wrap the same sentinel.
func markWebSocketTransportReadEOF(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.Join(errWebSocketTransportReadEOF, err)
	}
	return err
}

func isWebSocketDisconnect(err error) bool {
	if errors.Is(err, errWebSocketTransportReadEOF) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	if isDisconnect(err) {
		return true
	}
	return websocket.CloseStatus(err) != -1
}

// webSocketDrainReadDeadlineConn lets the ordinary frame-read timeout expire
// during service, then makes an outstanding read indefinite once shutdown has
// committed to draining this connection. NetConn closes the whole WebSocket
// when a deadline fires during an active read, so the deadline must be cleared
// before the idle peer can interrupt queued output.
type webSocketDrainReadDeadlineConn struct {
	net.Conn

	readDeadlineMu       sync.Mutex
	preserveReadDeadline bool
}

func (c *webSocketDrainReadDeadlineConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	return n, markWebSocketTransportReadEOF(err)
}

func (c *webSocketDrainReadDeadlineConn) SetDeadline(deadline time.Time) error {
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.SetReadDeadline(deadline)
}

func (c *webSocketDrainReadDeadlineConn) SetReadDeadline(deadline time.Time) error {
	c.readDeadlineMu.Lock()
	defer c.readDeadlineMu.Unlock()
	if c.preserveReadDeadline {
		deadline = time.Time{}
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *webSocketDrainReadDeadlineConn) preserveReadDeadlineForDrain() error {
	c.readDeadlineMu.Lock()
	defer c.readDeadlineMu.Unlock()
	c.preserveReadDeadline = true
	return c.Conn.SetReadDeadline(time.Time{})
}

// webSocketAcceptedConn carries the pre-auth slot from the HTTP server's
// accepted socket into the WebSocket handler. Closing the socket releases the
// slot even when the request never upgrades.
type webSocketAcceptedConn struct {
	net.Conn

	slot            *preAuthSlot
	listener        *webSocketListener
	serving         atomic.Bool
	handlerTracked  atomic.Bool
	handlerStarted  atomic.Bool
	handlerDoneOnce sync.Once
	// Lock order: publishConnectionIdentity takes pendingMu before state.
	// prepare may hold state while keyAddr acquires the pre-auth limiter mutex.
	// Close releases state before slot.clear and untrack, so neither order is
	// reversed.
	state  sync.Mutex
	closed bool
	// preAuthClosing is protected by webSocketListener.pendingMu. It prevents
	// the pre-auth lifetime ceiling from racing a connection into serving.
	preAuthClosing bool
	addr           netip.Addr
	deadline       time.Time
	connectionID   string
	openedAt       time.Time
	// lifecycleCloseCauseSet distinguishes an explicit nil server-close cause
	// from a socket closed by the peer.
	lifecycleCloseCause    error
	lifecycleCloseCauseSet bool
}

func (c *webSocketAcceptedConn) Close() error {
	return c.close(false, nil)
}

func (c *webSocketAcceptedConn) closeWithLifecycleCause(cause error) error {
	return c.close(true, cause)
}

func (c *webSocketAcceptedConn) close(setLifecycleCause bool, cause error) error {
	c.state.Lock()
	if c.closed {
		c.state.Unlock()
		return nil
	}
	if setLifecycleCause {
		c.lifecycleCloseCause = cause
		c.lifecycleCloseCauseSet = true
	}
	c.closed = true
	c.state.Unlock()
	c.slot.clear()
	err := c.Conn.Close()
	if c.listener != nil {
		c.listener.untrack(c)
	}
	return err
}

func (c *webSocketAcceptedConn) lifecycleCloseCauseValue() (error, bool) {
	c.state.Lock()
	defer c.state.Unlock()
	return c.lifecycleCloseCause, c.lifecycleCloseCauseSet
}

// publishConnectionIdentity gives lifecycle callbacks the identity before the
// HTTP upgrade starts, without holding the close mutex during upgrade I/O.
func (c *webSocketAcceptedConn) publishConnectionIdentity(connectionID string, openedAt time.Time) bool {
	if c.listener != nil {
		c.listener.pendingMu.Lock()
		if _, tracked := c.listener.pending[c]; !tracked || c.preAuthClosing {
			c.listener.pendingMu.Unlock()
			return false
		}
		c.state.Lock()
		if c.closed {
			c.state.Unlock()
			c.listener.pendingMu.Unlock()
			return false
		}
		c.connectionID = connectionID
		c.openedAt = openedAt
		c.state.Unlock()
		c.listener.pendingMu.Unlock()
		return true
	}
	c.state.Lock()
	defer c.state.Unlock()
	if c.closed {
		return false
	}
	c.connectionID = connectionID
	c.openedAt = openedAt
	return true
}

// logOpenedIfActive orders the open record before any close record while
// keeping the HTTP upgrade itself outside the close mutex.
func (c *webSocketAcceptedConn) logOpenedIfActive(logOpen func()) bool {
	c.state.Lock()
	defer c.state.Unlock()
	if c.closed {
		return false
	}
	logOpen()
	return true
}

func (c *webSocketAcceptedConn) identityLogAttrs() []any {
	c.state.Lock()
	defer c.state.Unlock()
	return connectionIdentityLogAttrs(c.connectionID, c.openedAt)
}

func (c *webSocketAcceptedConn) markServing() bool {
	if c.listener == nil {
		c.serving.Store(true)
		return true
	}
	return c.listener.markServing(c)
}

func (c *webSocketAcceptedConn) handlerDone() {
	if c.listener == nil || !c.handlerTracked.Load() {
		return
	}
	c.handlerDoneOnce.Do(func() { c.listener.handlers.Done() })
}

// webSocketListener admits sockets before net/http reads their HTTP request.
// The raw accept loop only admits and dispatches sockets; every client-byte
// read, including PROXY-v2 address establishment, happens in a worker before
// that socket is made visible to net/http. This keeps a silent peer from
// holding the goroutine that accepts the next HTTP connection.
type webSocketListener struct {
	net.Listener

	server            *Server
	rateLimitListener store.RateLimitListener
	clientAddrHeader  string
	ready             chan *webSocketAcceptedConn
	acceptErr         chan error
	done              chan struct{}
	acceptDone        chan struct{}
	closeOnce         sync.Once

	// pendingMu protects pending and preAuthClosing. publishConnectionIdentity
	// nests accepted.state beneath it; no path acquires pendingMu while holding
	// state. It is released before pre-auth, socket, or server operations.
	pendingMu sync.Mutex
	pending   map[*webSocketAcceptedConn]struct{}
	live      sync.WaitGroup
	handlers  sync.WaitGroup
}

func newWebSocketListener(listener net.Listener, server *Server, rateLimitListener store.RateLimitListener, clientAddrHeader string) *webSocketListener {
	return &webSocketListener{
		Listener:          listener,
		server:            server,
		rateLimitListener: rateLimitListener,
		clientAddrHeader:  clientAddrHeader,
		ready:             make(chan *webSocketAcceptedConn, 1),
		acceptErr:         make(chan error, 1),
		done:              make(chan struct{}),
		acceptDone:        make(chan struct{}),
		pending:           make(map[*webSocketAcceptedConn]struct{}),
	}
}

func (l *webSocketListener) start() {
	go l.acceptLoop()
}

func (l *webSocketListener) Accept() (net.Conn, error) {
	select {
	case err := <-l.acceptErr:
		return nil, err
	default:
	}
	select {
	case accepted := <-l.ready:
		if !l.trackHandler(accepted) {
			l.closeAccepted(accepted)
			return nil, net.ErrClosed
		}
		return accepted, nil
	case err := <-l.acceptErr:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *webSocketListener) acceptLoop() {
	defer close(l.acceptDone)
	var backoff time.Duration
	for {
		sock, err := l.Listener.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if isTransientAccept(err) {
				backoff = nextAcceptBackoff(backoff)
				if backoff >= maxAcceptBackoff {
					l.server.log.Warn("WebSocket accept still failing at maximum backoff", "err", err)
				} else {
					l.server.log.Info("WebSocket accept failed, retrying", "err", err)
				}
				select {
				case <-l.done:
					return
				case <-time.After(backoff):
				}
				continue
			}
			select {
			case l.acceptErr <- errors.Join(errors.New("accept WebSocket connection"), err):
			default:
			}
			return
		}
		backoff = 0

		slot, ok := l.server.preAuth.admit(l.rateLimitListener)
		if !ok {
			l.server.dropRefused(sock, "", time.Time{})
			if dropped, allow := l.server.globalCapLog.allow(time.Now(), preAuthLogInterval); allow {
				l.server.log.Info("WebSocket connection refused at the pre-auth cap",
					"cap", l.server.preAuth.limits.MaxConns, "suppressed", dropped)
			}
			continue
		}
		accepted := &webSocketAcceptedConn{
			Conn:     sock,
			slot:     slot,
			listener: l,
			deadline: time.Now().Add(l.server.handshakeTimeout),
		}
		if err := accepted.SetDeadline(accepted.deadline); err != nil {
			l.server.logNegotiation(errors.Join(errors.New("set WebSocket negotiation deadline"), err), nil)
			l.closeAccepted(accepted)
			continue
		}
		l.armLifetime(accepted)
		if !l.track(accepted) {
			l.closeAccepted(accepted)
			return
		}
		go l.prepare(accepted)
	}
}

func (l *webSocketListener) armLifetime(accepted *webSocketAcceptedConn) {
	accepted.slot.armLifetime(func() {
		l.pendingMu.Lock()
		if _, tracked := l.pending[accepted]; !tracked || accepted.serving.Load() || accepted.preAuthClosing {
			l.pendingMu.Unlock()
			return
		}
		accepted.preAuthClosing = true
		l.pendingMu.Unlock()

		if err := accepted.closeWithLifecycleCause(context.DeadlineExceeded); err != nil && !isDisconnect(err) {
			attrs := append(accepted.identityLogAttrs(), "err", err)
			l.server.log.Info("close WebSocket connection at the pre-auth ceiling", attrs...)
		}
		if dropped, ok := l.server.ceilingLog.allow(time.Now(), preAuthLogInterval); ok {
			attrs := accepted.identityLogAttrs()
			attrs = append(attrs, "lifetime", l.server.preAuth.limits.Lifetime, "suppressed", dropped)
			l.server.log.Info("WebSocket connection closed at the pre-auth lifetime ceiling", attrs...)
		}
	})
}

func (l *webSocketListener) prepare(accepted *webSocketAcceptedConn) {
	var addr netip.Addr
	if l.clientAddrHeader == "" {
		var err error
		addr, err = l.server.clientAddrUntil(accepted, accepted.deadline)
		if err != nil {
			l.server.logNegotiation(err, nil)
			l.closeAccepted(accepted)
			l.untrack(accepted)
			return
		}
	}
	accepted.state.Lock()
	if accepted.closed {
		accepted.state.Unlock()
		l.untrack(accepted)
		return
	}
	if l.clientAddrHeader == "" && !accepted.slot.keyAddr(addr) {
		accepted.state.Unlock()
		l.server.dropRefused(accepted, "", time.Time{})
		accepted.slot.clear()
		l.untrack(accepted)
		if dropped, ok := l.server.netCapLog.allow(time.Now(), preAuthLogInterval); ok {
			l.server.log.Info("WebSocket connection refused at the per-network pre-auth cap",
				"client_addr", addr, "cap", l.server.preAuth.limits.MaxConnsPerNet, "suppressed", dropped)
		}
		return
	}
	accepted.addr = addr
	delivered := false
	select {
	case l.ready <- accepted:
		delivered = true
	case <-l.done:
	}
	accepted.state.Unlock()
	if !delivered {
		l.closeAccepted(accepted)
		l.untrack(accepted)
	}
}

func (l *webSocketListener) closeAccepted(accepted *webSocketAcceptedConn) {
	if err := accepted.Close(); err != nil && !isDisconnect(err) {
		l.server.log.Info("close WebSocket connection", "err", err)
	}
}

func (l *webSocketListener) track(accepted *webSocketAcceptedConn) bool {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	select {
	case <-l.done:
		return false
	default:
	}
	l.pending[accepted] = struct{}{}
	l.live.Add(1)
	return true
}

func (l *webSocketListener) trackHandler(accepted *webSocketAcceptedConn) bool {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	select {
	case <-l.done:
		return false
	default:
	}
	if _, ok := l.pending[accepted]; !ok {
		return false
	}
	accepted.handlerTracked.Store(true)
	l.handlers.Add(1)
	return true
}

func (l *webSocketListener) untrack(accepted *webSocketAcceptedConn) {
	l.pendingMu.Lock()
	if _, ok := l.pending[accepted]; ok {
		delete(l.pending, accepted)
		l.live.Done()
	}
	l.pendingMu.Unlock()
}

func (l *webSocketListener) markServing(accepted *webSocketAcceptedConn) bool {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	// This callback runs for each encrypted frame, not just the first one. A
	// connection that entered the serving set before drain must keep flowing
	// through the retirement path even after the listener stops admitting new
	// connections.
	if accepted.serving.Load() {
		return true
	}
	if accepted.preAuthClosing {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
	}
	if _, ok := l.pending[accepted]; !ok {
		return false
	}
	accepted.serving.Store(true)
	return true
}

func (l *webSocketListener) Close() error {
	var closeErr error
	l.closeOnce.Do(func() {
		close(l.done)
		closeErr = l.Listener.Close()
		l.pendingMu.Lock()
		pending := make([]*webSocketAcceptedConn, 0, len(l.pending))
		for accepted := range l.pending {
			if !accepted.serving.Load() {
				pending = append(pending, accepted)
			}
		}
		l.pendingMu.Unlock()
		for _, accepted := range pending {
			if err := accepted.closeWithLifecycleCause(nil); err != nil && !isDisconnect(err) {
				l.server.log.Info("close WebSocket connection", "err", err)
			}
		}
	})
	return closeErr
}

func (l *webSocketListener) forceClose() {
	l.pendingMu.Lock()
	all := make([]*webSocketAcceptedConn, 0, len(l.pending))
	for accepted := range l.pending {
		if !accepted.serving.Load() {
			all = append(all, accepted)
		}
	}
	l.pendingMu.Unlock()
	for _, accepted := range all {
		if err := accepted.closeWithLifecycleCause(nil); err != nil && !isDisconnect(err) {
			l.server.log.Info("close WebSocket connection", "err", err)
		}
	}
}

func (l *webSocketListener) wait(ctx context.Context) bool {
	select {
	case <-l.acceptDone:
	case <-ctx.Done():
		return false
	}
	done := make(chan struct{})
	go func() {
		l.live.Wait()
		l.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// ServeWebSocket serves MTProto over WebSocket on l. It is deliberately a
// separate listener: the raw TCP listener keeps its existing transport
// detection, while this endpoint performs the HTTP upgrade and then enters the
// same per-connection MTProto path.
func (s *Server) ServeWebSocket(ctx context.Context, l net.Listener) error {
	return s.serveWebSocket(ctx, l, s.handler, s.webSocketOriginPatterns, s.webSocketBrowserOnly, s.clientDiscoveryDocument, store.RateLimitListenerTailnet, "")
}

// ServeWebSocketWithHandler serves MTProto over WebSocket with a handler and
// browser-origin policy selected by the listener owner. The handler is fixed
// for every connection accepted on this listener and cannot be selected by a
// request.
func (s *Server) ServeWebSocketWithHandler(ctx context.Context, l net.Listener, handler Handler, originPatterns []string) error {
	if handler == nil {
		return errors.New("WebSocket handler is required")
	}
	return s.serveWebSocket(ctx, l, handler, append([]string(nil), originPatterns...), false, s.clientDiscoveryDocument, store.RateLimitListenerTailnet, "")
}

// ServeWebSocketWithHandlerAndDiscovery serves a listener with its own public
// discovery document. This keeps public-only client configuration off the
// default listener's discovery route.
func (s *Server) ServeWebSocketWithHandlerAndDiscovery(ctx context.Context, l net.Listener, handler Handler, originPatterns []string, doc discovery.Document) error {
	return s.serveWebSocketWithDiscovery(ctx, l, handler, originPatterns, doc, false, "")
}

// ServeWebSocketWithHandlerAndDiscoveryAndBrowserOnly serves a listener with
// its own browser-origin policy and discovery document. When browserOnly is
// true, a request must carry an Origin matching originPatterns.
func (s *Server) ServeWebSocketWithHandlerAndDiscoveryAndBrowserOnly(ctx context.Context, l net.Listener, handler Handler, originPatterns []string, doc discovery.Document, browserOnly bool) error {
	return s.serveWebSocketWithDiscovery(ctx, l, handler, originPatterns, doc, browserOnly, "")
}

func (s *Server) serveWebSocketWithDiscovery(ctx context.Context, l net.Listener, handler Handler, originPatterns []string, doc discovery.Document, browserOnly bool, clientAddrHeader string) error {
	if handler == nil {
		return errors.New("WebSocket handler is required")
	}
	data, err := discovery.MarshalDocument(doc)
	if err != nil {
		return fmt.Errorf("marshal listener client discovery document: %w", err)
	}
	return s.serveWebSocket(ctx, l, handler, append([]string(nil), originPatterns...), browserOnly, data, store.RateLimitListenerPublic, clientAddrHeader)
}

// ServePublicWebSocketWithCFConnectingIP serves a public WebSocket listener
// with its own discovery document and trusts the one CF-Connecting-IP header
// supplied by the loopback-only Cloudflare Tunnel path. Other listeners keep
// their socket peer address.
func (s *Server) ServePublicWebSocketWithCFConnectingIP(ctx context.Context, l net.Listener, handler Handler, originPatterns []string, doc discovery.Document) error {
	return s.ServePublicWebSocketWithCFConnectingIPAndBrowserOnly(ctx, l, handler, originPatterns, doc, false)
}

// ServePublicWebSocketWithCFConnectingIPAndBrowserOnly serves a public
// listener with its own discovery document and browser-only policy while
// trusting its single Cloudflare client-address header.
func (s *Server) ServePublicWebSocketWithCFConnectingIPAndBrowserOnly(ctx context.Context, l net.Listener, handler Handler, originPatterns []string, doc discovery.Document, browserOnly bool) error {
	return s.serveWebSocketWithDiscovery(ctx, l, handler, originPatterns, doc, browserOnly, cfConnectingIPHeader)
}

func (s *Server) serveWebSocket(ctx context.Context, l net.Listener, handler Handler, originPatterns []string, browserOnly bool, discoveryDocument []byte, rateLimitListener store.RateLimitListener, clientAddrHeader string) error {
	s.shutdown.startServing()
	defer s.shutdown.finishServing()
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if state, ok := r.Context().Value(webSocketConnKey{}).(*webSocketConnState); ok {
				state.socket.handlerStarted.Store(true)
				defer state.socket.handlerDone()
			}
			s.handleWebSocket(w, r, handler, originPatterns, browserOnly, discoveryDocument, clientAddrHeader)
		}),
		// The accepted connection already carries an absolute deadline that
		// covers address establishment, HTTP parsing, upgrade and codec
		// detection. A duration here would replace it with a second budget.
		ReadHeaderTimeout: 0,
		MaxHeaderBytes:    8192,
		BaseContext:       func(net.Listener) context.Context { return s.shutdown.requestCtx },
		ConnContext:       s.webSocketConnContext,
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				if accepted, ok := conn.(*webSocketAcceptedConn); ok && !accepted.handlerStarted.Load() {
					accepted.handlerDone()
				}
			}
		},
	}
	server.SetKeepAlivesEnabled(false)
	listener := newWebSocketListener(l, s, rateLimitListener, clientAddrHeader)
	listener.start()
	stopOnContext := context.AfterFunc(ctx, s.shutdown.beginDrain)
	defer stopOnContext()
	stopOnDrain := context.AfterFunc(s.shutdown.drainCtx, func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.log.Info("close WebSocket listener at shutdown", "err", err)
		}
	})
	defer stopOnDrain()
	stopOnCutoff := context.AfterFunc(s.shutdown.requestCtx, func() {
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Info("close WebSocket server at drain cutoff", "err", err)
		}
		listener.forceClose()
	})
	defer stopOnCutoff()
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.log.Info("close WebSocket listener", "err", err)
		}
	}()
	err := server.Serve(listener)
	s.shutdown.beginDrain()
	if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		err = errors.Join(err, fmt.Errorf("close WebSocket listener: %w", closeErr))
	}
	if shutdownErr := server.Shutdown(s.shutdown.requestCtx); shutdownErr != nil {
		if !errors.Is(shutdownErr, context.Canceled) {
			err = errors.Join(err, fmt.Errorf("shutdown WebSocket HTTP server: %w", shutdownErr))
		}
		if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			err = errors.Join(err, fmt.Errorf("close WebSocket HTTP server: %w", closeErr))
		}
	}
	if !listener.wait(s.shutdown.requestCtx) {
		listener.forceClose()
		listener.wait(context.Background())
	}
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// webSocketConnContext copies the address established by the listener worker.
// It must not read from conn: net/http invokes ConnContext on its accept path,
// and a PROXY-v2 read there would let one silent peer stall every later accept.
func (s *Server) webSocketConnContext(ctx context.Context, conn net.Conn) context.Context {
	accepted, ok := conn.(*webSocketAcceptedConn)
	if !ok {
		return ctx
	}

	state := &webSocketConnState{
		socket:   accepted,
		slot:     accepted.slot,
		addr:     accepted.addr,
		deadline: accepted.deadline,
	}
	return context.WithValue(ctx, webSocketConnKey{}, state)
}

// SetWebSocketOriginPatterns configures the browser origins accepted by the
// WebSocket endpoint. Call it before ServeWebSocket. Requests without an
// Origin header remain valid; an Origin header must match one of these
// patterns, and an empty list therefore accepts no browser origin.
func (s *Server) SetWebSocketOriginPatterns(patterns []string) {
	s.webSocketOriginPatterns = append([]string(nil), patterns...)
}

// SetWebSocketBrowserOnly requires an allow-listed Origin on the WebSocket
// endpoint. Call it before ServeWebSocket.
func (s *Server) SetWebSocketBrowserOnly(browserOnly bool) {
	s.webSocketBrowserOnly = browserOnly
}

// SetClientDiscoveryDocument configures the public identity served beside the
// WebSocket endpoint. Call it before ServeWebSocket.
func (s *Server) SetClientDiscoveryDocument(doc discovery.Document) error {
	data, err := discovery.MarshalDocument(doc)
	if err != nil {
		return fmt.Errorf("marshal client discovery document: %w", err)
	}
	s.clientDiscoveryDocument = append([]byte(nil), data...)
	return nil
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request, handler Handler, originPatterns []string, browserOnly bool, discoveryDocument []byte, clientAddrHeader string) {
	state, ok := r.Context().Value(webSocketConnKey{}).(*webSocketConnState)
	if !ok {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// Keep the slot tied to the request when the HTTP upgrade fails. The listener
	// worker closes address-establishment failures before they reach net/http.
	defer state.slot.clear()
	if browserOnly && r.URL.Path == websocketPath && !webSocketOriginAllowed(r, originPatterns, browserOnly) {
		w.Header().Set(clientPolicyHeader, "browser-only")
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if clientAddrHeader != "" {
		values := r.Header.Values(clientAddrHeader)
		if len(values) != 1 {
			s.logInvalidClientAddrHeader(len(values))
			http.Error(w, "CF-Connecting-IP must appear exactly once", http.StatusBadRequest)
			return
		}
		addr, err := netip.ParseAddr(values[0])
		if err != nil || !addr.IsValid() || addr.Zone() != "" {
			s.logInvalidClientAddrHeader(len(values))
			http.Error(w, "CF-Connecting-IP must be exactly one IP address", http.StatusBadRequest)
			return
		}
		addr = addr.Unmap()
		state.socket.state.Lock()
		if state.socket.closed {
			state.socket.state.Unlock()
			return
		}
		keyed := state.slot.keyAddr(addr)
		if keyed {
			state.addr = addr
		}
		state.socket.state.Unlock()
		if !keyed {
			if dropped, allow := s.netCapLog.allow(time.Now(), preAuthLogInterval); allow {
				s.log.Info("WebSocket connection refused at the per-network pre-auth cap",
					"client_addr", addr, "cap", s.preAuth.limits.MaxConnsPerNet, "suppressed", dropped)
			}
			http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
			return
		}
	}
	requestPath := r.URL.EscapedPath()
	if requestPath == clientDiscoveryPath {
		s.handleClientDiscovery(w, r, discoveryDocument)
		return
	}
	if r.URL.Path != websocketPath {
		w.Header().Set("Connection", "close")
		http.NotFound(w, r)
		return
	}
	if !webSocketOriginAllowed(r, originPatterns, browserOnly) {
		if browserOnly {
			w.Header().Set(clientPolicyHeader, "browser-only")
		}
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}

	connectionID := s.newConnectionID()
	openedAt := time.Now()
	if !state.socket.publishConnectionIdentity(connectionID, openedAt) {
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{"binary"},
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  originPatterns,
	})
	if err != nil {
		s.logNegotiation(err, nil)
		return
	}
	if !state.socket.logOpenedIfActive(func() {
		s.logConnectionOpened(websocketPath, connectionID, openedAt)
	}) {
		if err := ws.CloseNow(); err != nil && !isDisconnect(err) {
			s.log.Info("close WebSocket connection after concurrent close", "err", err)
		}
		return
	}
	closeDiagnostics := newTransportCloseDiagnostics(websocketPath, connectionID, openedAt)
	var closeErr error
	defer func() {
		if cause, set := state.socket.lifecycleCloseCauseValue(); set {
			closeErr = cause
		}
		s.logTransportClose(closeDiagnostics, closeErr)
	}()
	defer func() {
		if err := ws.CloseNow(); err != nil && !isDisconnect(err) {
			s.log.Info("close WebSocket connection", "err", err)
		}
		if err := state.socket.Close(); err != nil && !isDisconnect(err) {
			s.log.Info("close WebSocket socket", "err", err)
		}
	}()
	// net/http clears the connection deadline when it starts its background
	// reader, and the upgrade itself hands the socket to the WebSocket layer.
	// Restore the absolute deadline, never a fresh duration, before codec
	// detection; serveConn replaces it with the normal per-frame deadlines after
	// detection succeeds.
	if err := state.socket.SetDeadline(state.deadline); err != nil {
		closeErr = err
		s.logNegotiation(errors.Join(errors.New("set WebSocket negotiation deadline"), err), nil)
		return
	}
	stream := &webSocketDrainReadDeadlineConn{
		Conn: websocket.NetConn(context.Background(), ws, websocket.MessageBinary),
	}
	// NetConn disables the library's default message limit for generic tunnels;
	// restore a finite bound after creating that stream wrapper.
	ws.SetReadLimit(maxWebSocketMessageSize)
	hardCutoffDone := make(chan struct{})
	stopHardCutoff := context.AfterFunc(s.shutdown.requestCtx, func() {
		defer close(hardCutoffDone)
		if err := ws.CloseNow(); err != nil && !isDisconnect(err) {
			s.log.Info("close WebSocket connection at shutdown", "err", err)
		}
	})

	conn, err := s.detectCodec(stream)
	if !stopHardCutoff() {
		<-hardCutoffDone
	}
	if err != nil {
		closeErr = err
		s.logNegotiation(err, nil)
		return
	}
	if err := state.socket.SetDeadline(time.Time{}); err != nil {
		closeErr = err
		s.logNegotiation(errors.Join(errors.New("clear WebSocket negotiation deadline"), err), nil)
		return
	}
	drainingConn := webSocketDrainConn{Conn: conn, socket: ws, shutdown: s.shutdown, readDeadline: stream}
	closeErr = s.serveConnWithHandlerContextsAndTransportDiagnostics(s.shutdown.requestCtx, s.shutdown.requestCtx, drainingConn, state.addr, state.slot, func() bool {
		return state.socket.markServing()
	}, websocketPath, handler, closeDiagnostics)
	if closeErr != nil && !isWebSocketDisconnect(closeErr) {
		s.logConnectionFailure(closeErr, closeDiagnostics)
	}
}

func (s *Server) logInvalidClientAddrHeader(valueCount int) {
	if dropped, ok := s.clientAddrHeaderLog.allow(time.Now(), preAuthLogInterval); ok {
		s.log.Warn("invalid CF-Connecting-IP header",
			"header", cfConnectingIPHeader, "value_count", valueCount, "suppressed", dropped)
	}
}

func (s *Server) handleClientDiscovery(w http.ResponseWriter, r *http.Request, discoveryDocument []byte) {
	if r.Method != http.MethodGet && r.Method != http.MethodOptions {
		w.Header().Set("Allow", "GET, OPTIONS")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(discoveryDocument) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(discoveryDocument); err != nil {
		s.log.Info("write public client discovery document", "err", err)
	}
}

type webSocketDrainConn struct {
	transport.Conn

	socket       *websocket.Conn
	shutdown     *serverShutdown
	readDeadline *webSocketDrainReadDeadlineConn
}

func (c webSocketDrainConn) preserveReadDeadlineForDrain() error {
	if c.readDeadline == nil {
		return nil
	}
	return c.readDeadline.preserveReadDeadlineForDrain()
}

func (c webSocketDrainConn) Close() error {
	if c.shutdown.draining() {
		return c.socket.CloseNow()
	}
	return c.Conn.Close()
}

// webSocketOriginAllowed applies the explicit browser-origin policy before the
// WebSocket library's check. The library always allows an Origin matching the
// request Host, which is useful as a default but would make an unset policy
// accept a browser request. The server's policy is fail-closed: an Origin must
// match a configured pattern. No Origin is valid for native clients unless the
// listener is configured as browser-only.
func webSocketOriginAllowed(r *http.Request, patterns []string, browserOnly bool) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return !browserOnly
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, pattern := range patterns {
		target := u.Host
		if strings.Contains(pattern, "://") {
			target = u.Scheme + "://" + u.Host
		}
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(target))
		if err == nil && matched {
			return true
		}
	}
	return false
}
