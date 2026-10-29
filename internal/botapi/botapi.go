package botapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	maxRequestBodyBytes = 64 << 10
	authBurst           = 20
	authRefillInterval  = time.Second
	maxAuthNetworks     = 4096
	authNetworkIdleTTL  = 2 * time.Minute
	readHeaderTimeout   = 5 * time.Second
	maxHeaderBytes      = 8192
)

// DefaultMaxConnectionsPerIP is the Bot API listener's per-source connection cap.
const DefaultMaxConnectionsPerIP = 32

const (
	unauthorizedBody    = "{\"ok\":false,\"error_code\":401,\"description\":\"Unauthorized\"}\n"
	tooManyRequestsBody = "{\"ok\":false,\"error_code\":429,\"description\":\"Too Many Requests: retry after 1\",\"parameters\":{\"retry_after\":1}}\n"
	badRequestBody      = "{\"ok\":false,\"error_code\":400,\"description\":\"Bad Request\"}\n"
	tooLargeBody        = "{\"ok\":false,\"error_code\":413,\"description\":\"Request Entity Too Large\"}\n"
	notFoundBody        = "{\"ok\":false,\"error_code\":404,\"description\":\"Not Found\"}\n"
	serverErrorBody     = "{\"ok\":false,\"error_code\":500,\"description\":\"Internal Server Error\"}\n"
)

// Store is the server-side identity lookup used by the Bot API listener.
type Store interface {
	BotIDByToken(context.Context, string) (int64, error)
	UserByID(context.Context, int64) (store.User, bool, error)
}

type handler struct {
	store   Store
	log     *slog.Logger
	limiter *authLimiter
}

type successResponse struct {
	OK     bool       `json:"ok"`
	Result botAPIUser `json:"result"`
}

type botAPIUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// NewHandler builds the independent HTTP router for Bot API requests.
func NewHandler(st Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &handler{
		store:   st,
		log:     logger,
		limiter: newAuthLimiter(),
	}
}

// NewServer creates an HTTP server dedicated to the Bot API listener.
func NewServer(addr string, st Store, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(st, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(errorLogWriter{logger: logger}, "", 0),
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var botID int64
	method := "unknown"
	defer func() {
		if recover() != nil {
			h.log.Error("Bot API handler panic", "bot_id", botID, "method", method)
			h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		}
	}()

	if !h.limiter.reserve(r.RemoteAddr, time.Now()) {
		h.rateLimited(w, botID, method)
		return
	}
	authAttemptReserved := true
	defer func() {
		if authAttemptReserved {
			h.limiter.refund(r.RemoteAddr, time.Now())
		}
	}()
	unauthorized := func(id int64, requestMethod string) {
		authAttemptReserved = false
		h.unauthorized(w, id, requestMethod)
	}
	token, endpointMethod, endpoint := parseEndpoint(r.URL.Path)
	if !endpoint {
		_, bodyStatus := requestBody(w, r)
		if bodyStatus != 0 {
			if bodyStatus == http.StatusRequestEntityTooLarge {
				h.write(w, bodyStatus, []byte(tooLargeBody), botID, method)
			} else {
				h.write(w, bodyStatus, []byte(badRequestBody), botID, method)
			}
			return
		}
		unauthorized(botID, method)
		return
	}
	method = methodLabel(endpointMethod)

	botID, err := h.store.BotIDByToken(r.Context(), token)
	if errors.Is(err, store.ErrBotTokenNotFound) {
		unauthorized(botID, method)
		return
	}
	if err != nil {
		h.log.Error("Bot API token lookup failed", "bot_id", 0, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), 0, method)
		return
	}
	if botID <= 0 {
		unauthorized(0, method)
		return
	}

	bodyPresent, bodyStatus := requestBody(w, r)
	if bodyStatus != 0 {
		if bodyStatus == http.StatusRequestEntityTooLarge {
			h.log.Info("Bot API request", "bot_id", botID, "method", method)
			h.write(w, bodyStatus, []byte(tooLargeBody), botID, method)
			return
		}
		h.log.Info("Bot API request", "bot_id", botID, "method", method)
		h.write(w, bodyStatus, []byte(badRequestBody), botID, method)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		h.log.Info("Bot API request", "bot_id", botID, "method", method)
		h.write(w, http.StatusNotFound, []byte(notFoundBody), botID, method)
		return
	}
	if endpointMethod != "getMe" {
		h.log.Info("Bot API request", "bot_id", botID, "method", method)
		h.write(w, http.StatusNotFound, []byte(notFoundBody), botID, method)
		return
	}
	if r.URL.RawQuery != "" || bodyPresent {
		h.log.Info("Bot API request", "bot_id", botID, "method", method)
		h.write(w, http.StatusBadRequest, []byte(badRequestBody), botID, method)
		return
	}

	user, exists, err := h.store.UserByID(r.Context(), botID)
	if err != nil {
		h.log.Error("Bot API profile lookup failed", "bot_id", botID, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	if !exists {
		unauthorized(botID, method)
		return
	}
	username := ""
	if user.Username != nil {
		username = *user.Username
	}
	response := successResponse{
		OK: true,
		Result: botAPIUser{
			ID:        botID,
			IsBot:     true,
			FirstName: user.FirstName,
			Username:  username,
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		h.log.Error("Bot API response encoding failed", "bot_id", botID, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	encoded = append(encoded, '\n')
	h.log.Info("Bot API request", "bot_id", botID, "method", method)
	h.write(w, http.StatusOK, encoded, botID, method)
}

func parseEndpoint(path string) (token, method string, ok bool) {
	if !strings.HasPrefix(path, "/bot") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, "/bot")
	token, method, found := strings.Cut(rest, "/")
	if !found {
		return rest, "", true
	}
	return token, method, true
}

func methodLabel(method string) string {
	if len(method) == 0 || len(method) > 64 {
		return "unknown"
	}
	for _, r := range method {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return "unknown"
		}
	}
	return method
}

func requestBody(w http.ResponseWriter, r *http.Request) (present bool, status int) {
	if r.Body == nil {
		return false, 0
	}
	if r.ContentLength > maxRequestBodyBytes {
		return false, http.StatusRequestEntityTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		if maxBytesErr, ok := errors.AsType[*http.MaxBytesError](err); ok && maxBytesErr != nil {
			return false, http.StatusRequestEntityTooLarge
		}
		return false, http.StatusBadRequest
	}
	return len(body) != 0, 0
}

func (h *handler) unauthorized(w http.ResponseWriter, botID int64, method string) {
	h.log.Warn("Bot API authentication failed", "bot_id", botID, "method", method)
	h.write(w, http.StatusUnauthorized, []byte(unauthorizedBody), botID, method)
}

func (h *handler) rateLimited(w http.ResponseWriter, botID int64, method string) {
	h.log.Warn("Bot API rate limit exceeded", "bot_id", botID, "method", method)
	h.write(w, http.StatusTooManyRequests, []byte(tooManyRequestsBody), botID, method)
}

func (h *handler) write(w http.ResponseWriter, status int, body []byte, botID int64, method string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		h.log.Error("Bot API response write failed", "bot_id", botID, "method", method)
	}
}

type errorLogWriter struct {
	logger *slog.Logger
}

func (w errorLogWriter) Write(message []byte) (int, error) {
	w.logger.Error("Bot API HTTP server error", "bot_id", 0, "method", "unknown")
	return len(message), nil
}

type authBucket struct {
	tokens     int
	lastRefill time.Time
	lastSeen   time.Time
}

type authLimiter struct {
	mu       sync.Mutex
	buckets  map[string]authBucket
	overflow authBucket
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{buckets: make(map[string]authBucket)}
}

func (l *authLimiter) reserve(remoteAddr string, now time.Time) bool {
	return l.update(remoteAddr, now, true)
}

func (l *authLimiter) refund(remoteAddr string, now time.Time) {
	l.update(remoteAddr, now, false)
}

func (l *authLimiter) update(remoteAddr string, now time.Time, reserve bool) bool {
	key := sourceNetwork(remoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.buckets[key]
	if exists && now.Sub(bucket.lastSeen) >= authNetworkIdleTTL {
		delete(l.buckets, key)
		exists = false
	}
	if !exists {
		for network, candidate := range l.buckets {
			if now.Sub(candidate.lastSeen) >= authNetworkIdleTTL {
				delete(l.buckets, network)
			}
		}
		if len(l.buckets) < maxAuthNetworks {
			bucket = authBucket{tokens: authBurst, lastRefill: now}
			exists = true
		}
	}
	if !exists {
		bucket = l.overflow
		if bucket.lastSeen.IsZero() || now.Sub(bucket.lastSeen) >= authNetworkIdleTTL {
			bucket = authBucket{tokens: authBurst, lastRefill: now}
		}
	}

	if elapsed := now.Sub(bucket.lastRefill); elapsed >= authRefillInterval {
		refill := int(elapsed / authRefillInterval)
		bucket.tokens += refill
		if bucket.tokens > authBurst {
			bucket.tokens = authBurst
		}
		bucket.lastRefill = bucket.lastRefill.Add(time.Duration(refill) * authRefillInterval)
	}
	bucket.lastSeen = now
	if reserve {
		if bucket.tokens == 0 {
			if exists {
				l.buckets[key] = bucket
			} else {
				l.overflow = bucket
			}
			return false
		}
		bucket.tokens--
	} else if bucket.tokens < authBurst {
		bucket.tokens++
	}
	if exists {
		l.buckets[key] = bucket
	} else {
		l.overflow = bucket
	}
	return true
}

func sourceNetwork(remoteAddr string) string {
	ip := remoteIP(remoteAddr)
	if !ip.IsValid() {
		return "unknown"
	}
	bits := 64
	if ip.Is4() {
		bits = 24
	}
	return netip.PrefixFrom(ip, bits).Masked().String()
}

func remoteIP(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		remoteAddr = host
	}
	ip, err := netip.ParseAddr(remoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ip.WithZone("").Unmap()
}

type connectionLimitListener struct {
	net.Listener

	mu     sync.Mutex
	limit  int
	active map[netip.Addr]int
}

func limitConnectionsPerIP(listener net.Listener, limit int) net.Listener {
	if limit < 1 {
		limit = DefaultMaxConnectionsPerIP
	}
	return &connectionLimitListener{
		Listener: listener,
		limit:    limit,
		active:   make(map[netip.Addr]int),
	}
}

// LimitConnectionsPerIP bounds the number of open HTTP connections from one
// source IP. Rejected connections are closed before the HTTP server sees them.
func LimitConnectionsPerIP(listener net.Listener, limit int) net.Listener {
	return limitConnectionsPerIP(listener, limit)
}

func (l *connectionLimitListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := remoteIP(conn.RemoteAddr().String())
		l.mu.Lock()
		if l.active[ip] >= l.limit {
			l.mu.Unlock()
			if err := conn.Close(); err != nil {
				return nil, err
			}
			continue
		}
		l.active[ip]++
		l.mu.Unlock()
		return &limitedConn{Conn: conn, release: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.active[ip]--
			if l.active[ip] == 0 {
				delete(l.active, ip)
			}
		}}, nil
	}
}

type limitedConn struct {
	net.Conn

	once    sync.Once
	release func()
	err     error
}

func (c *limitedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.release()
	})
	return c.err
}
