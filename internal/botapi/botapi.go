package botapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	forbiddenBody       = "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot can't initiate conversation with a user\"}\n"
	botBlockedBody      = "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot was blocked by the user\"}\n"
	messageSendLimit    = 4096
)

// Store is the server-side identity lookup used by the Bot API listener.
type Store interface {
	BotIDByToken(context.Context, string) (int64, error)
	UserByID(context.Context, int64) (store.User, bool, error)
	CheckRateLimit(context.Context, int64, string, store.RateLimitConfig) (*store.RateLimitResult, error)
	SendBotMessage(context.Context, int64, int64, string, int64) (store.Message, int, int, error)
	Notify(context.Context, string, string) error
	BotUpdateOffset(context.Context, int64, string) (int64, error)
	StartBotUpdatePoll(context.Context, int64, string) (store.BotUpdatePoll, error)
	CheckBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration) error
	FinishBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration, int64) (int64, error)
	BotMessageEvents(context.Context, int64, int64, int) (store.BotMessageEventPage, error)
}

type handler struct {
	store                Store
	log                  *slog.Logger
	limiter              *authLimiter
	messageSendRateLimit store.RateLimitConfig
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

type sendMessageResponse struct {
	OK     bool          `json:"ok"`
	Result botAPIMessage `json:"result"`
}

type botAPIMessage struct {
	MessageID int64      `json:"message_id"`
	From      botAPIUser `json:"from"`
	Chat      botAPIChat `json:"chat"`
	Date      int64      `json:"date"`
	Text      string     `json:"text"`
}

type botAPIChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// NewHandler builds the independent HTTP router for Bot API requests.
func NewHandler(st Store, logger *slog.Logger, messageSendRateLimit store.RateLimitConfig) *handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &handler{
		store:                st,
		log:                  logger,
		limiter:              newAuthLimiter(),
		messageSendRateLimit: messageSendRateLimit,
	}
}

// NewServer creates an HTTP server dedicated to the Bot API listener.
func NewServer(addr string, st Store, logger *slog.Logger, messageSendRateLimit store.RateLimitConfig) *http.Server {
	return NewServerWithHandler(addr, NewHandler(st, logger, messageSendRateLimit), logger)
}

// NewServerWithHandler creates a Bot API HTTP server around a shared handler.
// The shared handler also receives update-log wakeups from the store listener.
func NewServerWithHandler(addr string, h http.Handler, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      getUpdatesWriteTimeout,
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

	body, bodyStatus := requestBody(w, r)
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
	if endpointMethod != "getMe" && endpointMethod != "sendMessage" && endpointMethod != "getUpdates" {
		h.log.Info("Bot API request", "bot_id", botID, "method", method)
		h.write(w, http.StatusNotFound, []byte(notFoundBody), botID, method)
		return
	}
	if endpointMethod == "getMe" && (r.URL.RawQuery != "" || len(body) != 0) {
		h.log.Info("Bot API request", "bot_id", botID, "method", method)
		h.write(w, http.StatusBadRequest, []byte(badRequestBody), botID, method)
		return
	}
	if endpointMethod == "sendMessage" {
		if r.Method == http.MethodGet && len(body) != 0 {
			h.log.Info("Bot API request", "bot_id", botID, "method", method)
			h.write(w, http.StatusBadRequest, []byte(badRequestBody), botID, method)
			return
		}
		chatID, text, err := parseSendMessageRequest(r, body)
		if err != nil {
			h.log.Info("Bot API request", "bot_id", botID, "method", method)
			h.write(w, http.StatusBadRequest, []byte(badRequestBody), botID, method)
			return
		}
		h.sendMessage(w, r, botID, method, chatID, text)
		return
	}
	if endpointMethod == "getUpdates" {
		params, err := parseGetUpdatesRequest(r, body)
		if err != nil {
			h.log.Info("Bot API request", "bot_id", botID, "method", method, "err", err)
			h.write(w, http.StatusBadRequest, []byte(badRequestBody), botID, method)
			return
		}
		h.getUpdates(w, r, botID, method, token, params)
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

func (h *handler) sendMessage(w http.ResponseWriter, r *http.Request, botID int64, method string, chatID int64, text string) {
	user, exists, err := h.store.UserByID(r.Context(), botID)
	if err != nil {
		h.log.Error("Bot API profile lookup failed", "bot_id", botID, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	if !exists || user.Kind != store.UserKindBot {
		h.unauthorized(w, botID, method)
		return
	}

	limit, err := h.store.CheckRateLimit(r.Context(), botID, "message_send", h.messageSendRateLimit)
	if err != nil {
		h.log.Error("Bot API rate limit check failed", "bot_id", botID, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	if limit != nil {
		body := rateLimitedMessageBody(limit.Wait)
		h.log.Warn("Bot API rate limit exceeded", "bot_id", botID, "method", method)
		h.write(w, http.StatusTooManyRequests, body, botID, method)
		return
	}

	randomID, err := randomMessageID()
	if err != nil {
		h.log.Error("Bot API random id generation failed", "bot_id", botID, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	message, _, recipientPts, err := h.store.SendBotMessage(r.Context(), botID, chatID, text, randomID)
	switch {
	case errors.Is(err, store.ErrBotRecipientForbidden) || errors.Is(err, store.ErrYouBlockedUser):
		h.write(w, http.StatusForbidden, []byte(forbiddenBody), botID, method)
		return
	case errors.Is(err, store.ErrBotBlockedByUser):
		h.write(w, http.StatusForbidden, []byte(botBlockedBody), botID, method)
		return
	case err != nil:
		h.log.Error("Bot API message send failed", "bot_id", botID, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	if message.PeerLocalID <= 0 {
		h.log.Error("Bot API message has no recipient id", "bot_id", botID, "method", method)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	if recipientPts > 0 {
		notifyCtx, cancel := store.NotificationContext(r.Context())
		defer cancel()
		if err := h.store.Notify(notifyCtx, store.ChannelUpdates, strconv.FormatInt(chatID, 10)); err != nil {
			h.log.Error("Bot API message notify failed", "bot_id", botID, "method", method)
		}
	}

	username := ""
	if user.Username != nil {
		username = *user.Username
	}
	response := sendMessageResponse{
		OK: true,
		Result: botAPIMessage{
			MessageID: message.PeerLocalID,
			From: botAPIUser{
				ID:        botID,
				IsBot:     true,
				FirstName: user.FirstName,
				Username:  username,
			},
			Chat: botAPIChat{ID: chatID, Type: "private"},
			Date: message.Date.Unix(),
			Text: message.Text,
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

func parseSendMessageRequest(r *http.Request, body []byte) (int64, string, error) {
	if len(body) == 0 {
		return parseFormMessageValues(r.URL.RawQuery)
	}
	if r.URL.RawQuery != "" {
		return 0, "", errors.New("sendMessage cannot mix query and body parameters")
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return 0, "", errors.New("invalid sendMessage content type")
	}
	switch contentType {
	case "application/json":
		return parseJSONMessage(body)
	case "application/x-www-form-urlencoded":
		return parseFormMessageValues(string(body))
	default:
		return 0, "", errors.New("unsupported sendMessage content type")
	}
}

func parseJSONMessage(body []byte) (int64, string, error) {
	if !utf8.Valid(body) {
		return 0, "", errors.New("invalid UTF-8 in sendMessage body")
	}
	var request struct {
		ChatID json.RawMessage `json:"chat_id"`
		Text   json.RawMessage `json:"text"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return 0, "", errors.New("invalid sendMessage JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return 0, "", errors.New("trailing sendMessage JSON")
	}
	if len(request.ChatID) == 0 || len(request.Text) == 0 {
		return 0, "", errors.New("missing sendMessage field")
	}
	chatIDText := strings.TrimSpace(string(request.ChatID))
	if chatIDText == "" || (chatIDText[0] != '-' && (chatIDText[0] < '0' || chatIDText[0] > '9')) {
		return 0, "", errors.New("chat_id must be an integer")
	}
	chatID, err := strconv.ParseInt(chatIDText, 10, 64)
	if err != nil {
		return 0, "", errors.New("chat_id must be an integer")
	}
	var text string
	if err := json.Unmarshal(request.Text, &text); err != nil {
		return 0, "", errors.New("text must be a string")
	}
	if !validBotMessageText(text) {
		return 0, "", errors.New("invalid sendMessage text")
	}
	return chatID, text, nil
}

func parseFormMessageValues(encoded string) (int64, string, error) {
	values, err := url.ParseQuery(encoded)
	if err != nil {
		return 0, "", errors.New("invalid sendMessage form")
	}
	if len(values) != 2 || len(values["chat_id"]) != 1 || len(values["text"]) != 1 {
		return 0, "", errors.New("sendMessage requires one chat_id and one text")
	}
	for key := range values {
		if key != "chat_id" && key != "text" {
			return 0, "", errors.New("unsupported sendMessage field")
		}
	}
	chatID, err := strconv.ParseInt(values.Get("chat_id"), 10, 64)
	if err != nil {
		return 0, "", errors.New("chat_id must be an integer")
	}
	text := values.Get("text")
	if !validBotMessageText(text) {
		return 0, "", errors.New("invalid sendMessage text")
	}
	return chatID, text, nil
}

func validBotMessageText(text string) bool {
	if text == "" || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return false
	}
	units := 0
	for _, r := range text {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
		if units > messageSendLimit {
			return false
		}
	}
	return true
}

func randomMessageID() (int64, error) {
	var value [8]byte
	for {
		if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
			return 0, fmt.Errorf("read random message id: %w", err)
		}
		id := int64(binary.LittleEndian.Uint64(value[:]) & 0x7fffffffffffffff)
		if id != 0 {
			return id, nil
		}
	}
}

func rateLimitedMessageBody(wait time.Duration) []byte {
	retryAfter := int(wait / time.Second)
	if wait%time.Second != 0 {
		retryAfter++
	}
	if retryAfter < 1 {
		retryAfter = 1
	}
	return []byte(fmt.Sprintf("{\"ok\":false,\"error_code\":429,\"description\":\"Too Many Requests: retry after %d\",\"parameters\":{\"retry_after\":%d}}\n", retryAfter, retryAfter))
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

func requestBody(w http.ResponseWriter, r *http.Request) (body []byte, status int) {
	if r.Body == nil {
		return nil, 0
	}
	if r.ContentLength > maxRequestBodyBytes {
		return nil, http.StatusRequestEntityTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		if maxBytesErr, ok := errors.AsType[*http.MaxBytesError](err); ok && maxBytesErr != nil {
			return nil, http.StatusRequestEntityTooLarge
		}
		return nil, http.StatusBadRequest
	}
	return body, 0
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
	polls    map[int64]*updatePoll
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
