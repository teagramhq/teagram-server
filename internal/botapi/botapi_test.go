package botapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/botapi"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	requestBodyLimit = 64 << 10
	authAttemptLimit = 20
)

type fakeStore struct {
	lookupErrs  map[string]error
	lookupCount int
	lastUserID  int64
	user        store.User
	userExists  bool
	panicUser   bool
}

func (s *fakeStore) BotIDByToken(_ context.Context, token string) (int64, error) {
	s.lookupCount++
	if err := s.lookupErrs[token]; err != nil {
		return 0, err
	}
	return 123456, nil
}

func (s *fakeStore) UserByID(_ context.Context, id int64) (store.User, bool, error) {
	s.lastUserID = id
	if s.panicUser {
		panic("secret-sentinel from handler panic")
	}
	return s.user, s.userExists, nil
}

func newFakeStore() *fakeStore {
	username := "weather_bot"
	return &fakeStore{
		user:       store.User{ID: 123456, FirstName: "Weather", Username: &username},
		userExists: true,
	}
}

func TestGetMeReturnsServerLookedUpBot(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	const want = "{\"ok\":true,\"result\":{\"id\":123456,\"is_bot\":true,\"first_name\":\"Weather\",\"username\":\"weather_bot\"}}\n"
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request := httptest.NewRequestWithContext(context.Background(), method, "/bot999999:secret-sentinel/getMe", nil)
		request.RemoteAddr = "100.64.1.2:3456"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body %q", method, response.Code, response.Body.String())
		}
		if response.Body.String() != want {
			t.Fatalf("%s body = %q, want %q", method, response.Body.String(), want)
		}
	}
	if st.lastUserID != 123456 {
		t.Fatalf("UserByID called with %d, want token lookup id 123456", st.lastUserID)
	}
}

func TestValidGetMeRequestsDoNotConsumeUnauthorizedRateLimit(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	for i := range 25 {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:valid-secret/getMe", nil)
		request.RemoteAddr = "198.51.100.4:3456"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("valid request %d status = %d, want 200; body %q", i, response.Code, response.Body.String())
		}
	}
	if st.lookupCount != 25 {
		t.Fatalf("token lookups = %d, want 25", st.lookupCount)
	}
}

func TestMalformedUnknownAndRevokedTokensShareUnauthorizedBody(t *testing.T) {
	st := newFakeStore()
	st.lookupErrs = map[string]error{
		"malformed":      store.ErrBotTokenNotFound,
		"123456:unknown": store.ErrBotTokenNotFound,
		"123456:revoked": store.ErrBotTokenNotFound,
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	var firstBody string
	for i, token := range []string{"malformed", "123456:unknown", "123456:revoked"} {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot"+token+"/getMe", nil)
		request.RemoteAddr = "100.64.1.3:3456"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d, want 401", token, response.Code)
		}
		if i == 0 {
			firstBody = response.Body.String()
		} else if response.Body.String() != firstBody {
			t.Fatalf("token %q body = %q, want byte-identical %q", token, response.Body.String(), firstBody)
		}
	}
}

func TestUnknownBotAPIMethodReturnsBotAPI404(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates", nil)
	request.RemoteAddr = "100.64.1.4:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if got, want := response.Body.String(), "{\"ok\":false,\"error_code\":404,\"description\":\"Not Found\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestGetMeRejectsRequestBodyOverLimit(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	body := strings.NewReader(strings.Repeat("x", requestBodyLimit+1))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bot123456:secret-sentinel/getMe", body)
	request.RemoteAddr = "100.64.1.5:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body %q", response.Code, response.Body.String())
	}
}

func TestUnauthorizedRateLimitRunsBeforeTokenLookup(t *testing.T) {
	st := newFakeStore()
	st.lookupErrs = map[string]error{"bad-token": store.ErrBotTokenNotFound}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	for i := range authAttemptLimit {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/botbad-token/getMe", nil)
		request.RemoteAddr = "198.51.100.4:3456"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want 401", i, response.Code)
		}
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:valid-secret/getMe", nil)
	request.RemoteAddr = "198.51.100.4:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("valid request with empty auth budget status = %d, want 429", response.Code)
	}
	const wantBody = "{\"ok\":false,\"error_code\":429,\"description\":\"Too Many Requests: retry after 1\",\"parameters\":{\"retry_after\":1}}\n"
	if response.Body.String() != wantBody {
		t.Fatalf("rate limit body = %q, want %q", response.Body.String(), wantBody)
	}
	if st.lookupCount != authAttemptLimit {
		t.Fatalf("token lookups = %d, want %d; throttled request must stop before DB lookup", st.lookupCount, authAttemptLimit)
	}
}

func TestUnauthorizedRateLimitIsSharedBySourceNetwork(t *testing.T) {
	st := newFakeStore()
	st.lookupErrs = map[string]error{"bad-token": store.ErrBotTokenNotFound}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	for range authAttemptLimit {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/botbad-token/getMe", nil)
		request.RemoteAddr = "198.51.100.4:3456"
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/botbad-token/getMe", nil)
	request.RemoteAddr = "198.51.100.200:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("same-network status = %d, want 429", response.Code)
	}
	if st.lookupCount != authAttemptLimit {
		t.Fatalf("same-network token lookups = %d, want %d", st.lookupCount, authAttemptLimit)
	}
	request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/botbad-token/getMe", nil)
	request.RemoteAddr = "198.51.101.4:3456"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("different-network invalid token status = %d, want 401", response.Code)
	}
	if st.lookupCount != authAttemptLimit+1 {
		t.Fatalf("different-network token lookups = %d, want %d", st.lookupCount, authAttemptLimit+1)
	}
}

func TestAdminCookieDoesNotAuthenticateBotAPI(t *testing.T) {
	st := newFakeStore()
	st.lookupErrs = map[string]error{"admin-session-cookie": store.ErrBotTokenNotFound}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/botadmin-session-cookie/getMe", nil)
	request.RemoteAddr = "100.64.1.6:3456"
	request.AddCookie(&http.Cookie{ //nolint:gosec // G124: untrusted session cookie used to prove it is not Bot API auth
		Name:  "admin_session",
		Value: "admin-session-cookie",
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestBotAPIHandlerRedactsTokenFromLogsAndHTTPErrorLog(t *testing.T) {
	const token = "123456:secret-sentinel"
	const path = "/bot" + token + "/getMe"
	var logs bytes.Buffer
	var errorLog bytes.Buffer
	st := newFakeStore()
	st.lookupErrs = map[string]error{"unknown:secret-sentinel": store.ErrBotTokenNotFound}
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	server := botapi.NewServer("127.0.0.1:0", st, logger)
	server.ErrorLog = log.New(&errorLog, "", 0)
	testServer := httptest.NewUnstartedServer(server.Handler)
	testServer.Config.ErrorLog = server.ErrorLog
	testServer.Start()
	t.Cleanup(testServer.Close)

	request := func(method, requestPath string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method, testServer.URL+requestPath, body)
		if err != nil {
			t.Fatal("build HTTP request")
		}
		response, err := testServer.Client().Do(req)
		if err != nil {
			t.Fatalf("HTTP request failed for method %s", method)
		}
		return response
	}
	assertStatus := func(method, requestPath string, body io.Reader, wantStatus int) {
		t.Helper()
		response := request(method, requestPath, body)
		if err := response.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
		if response.StatusCode != wantStatus {
			t.Fatalf("status = %d, want %d", response.StatusCode, wantStatus)
		}
	}

	assertStatus(http.MethodGet, path, nil, http.StatusOK)
	assertStatus(http.MethodGet, "/botunknown:secret-sentinel/getMe", nil, http.StatusUnauthorized)
	assertStatus(http.MethodGet, "/bot"+token+"/notAMethod", nil, http.StatusNotFound)
	assertStatus(http.MethodGet, path+"?unexpected=secret-sentinel", nil, http.StatusBadRequest)
	st.panicUser = true
	assertStatus(http.MethodPost, path, nil, http.StatusInternalServerError)
	botapi.NewServer("127.0.0.1:0", st, logger).ErrorLog.Printf("request /bot%s/getMe failed", token)

	if strings.Contains(logs.String(), "secret-sentinel") || strings.Contains(logs.String(), "/bot") {
		t.Fatalf("slog output contains request secret or path: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "bot_id=123456") || !strings.Contains(logs.String(), "method=getMe") {
		t.Fatalf("slog output does not identify the bot and method: %q", logs.String())
	}
	if strings.Contains(errorLog.String(), "secret-sentinel") || strings.Contains(errorLog.String(), "/bot") {
		t.Fatalf("HTTP ErrorLog output contains request secret or path: %q", errorLog.String())
	}
}

func TestHTTPServerSetsRequestLimits(t *testing.T) {
	server := botapi.NewServer("127.0.0.1:0", newFakeStore(), slog.New(slog.DiscardHandler))
	if server.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want positive", server.ReadHeaderTimeout)
	}
	if server.MaxHeaderBytes <= 0 {
		t.Errorf("MaxHeaderBytes = %d, want positive", server.MaxHeaderBytes)
	}
	if server.ErrorLog == nil {
		t.Error("ErrorLog is nil")
	}
}

func TestConnectionCapRejectsConcurrentConnectionFromSameIP(t *testing.T) {
	var listenConfig net.ListenConfig
	base, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	limited := botapi.LimitConnectionsPerIP(base, 1)
	t.Cleanup(func() {
		if err := limited.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})

	var dialer net.Dialer
	firstClient, err := dialer.DialContext(context.Background(), "tcp", base.Addr().String())
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	t.Cleanup(func() {
		if err := firstClient.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close first client: %v", err)
		}
	})
	firstServer, err := limited.Accept()
	if err != nil {
		t.Fatalf("accept first: %v", err)
	}
	firstServerClosed := false
	t.Cleanup(func() {
		if !firstServerClosed {
			if err := firstServer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close first server connection: %v", err)
			}
		}
	})

	secondClient, err := dialer.DialContext(context.Background(), "tcp", base.Addr().String())
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	t.Cleanup(func() {
		if err := secondClient.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close second client: %v", err)
		}
	})
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := limited.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	if err := secondClient.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := secondClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("second connection remained open above the per-IP cap")
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Fatalf("second connection stayed open until deadline: %v", err)
	}
	if err := firstServer.Close(); err != nil {
		t.Fatalf("close first connection: %v", err)
	}
	firstServerClosed = true

	thirdClient, err := dialer.DialContext(context.Background(), "tcp", base.Addr().String())
	if err != nil {
		t.Fatalf("dial third: %v", err)
	}
	t.Cleanup(func() {
		if err := thirdClient.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close third client: %v", err)
		}
	})
	select {
	case conn := <-accepted:
		if err := conn.Close(); err != nil {
			t.Errorf("close accepted connection: %v", err)
		}
	case err := <-acceptErr:
		t.Fatalf("accept third: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("third connection was not accepted")
	}
}

func TestBotAPIWithoutBotTokenRejectsSessionOnlyRequest(t *testing.T) {
	handler := botapi.NewHandler(newFakeStore(), slog.New(slog.DiscardHandler))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/metrics", nil)
	request.RemoteAddr = "100.64.1.7:3456"
	request.AddCookie(&http.Cookie{ //nolint:gosec // G124: untrusted session cookie used to prove it is not Bot API auth
		Name:  "admin_session",
		Value: "valid-looking-session",
	})
	request.Header.Set("Authorization", "Bearer mtproto-user-session")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}
