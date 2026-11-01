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
	"sync"
	"sync/atomic"
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
	lookupErrs    map[string]error
	lookupCount   int
	lastUserID    int64
	user          store.User
	userExists    bool
	panicUser     bool
	sendCount     int
	sendBotID     int64
	sendChatID    int64
	sendText      string
	sendRandom    int64
	limitResult   *store.RateLimitResult
	limitErr      error
	limitCount    int
	limitUserID   int64
	limitSurface  string
	limitConfig   store.RateLimitConfig
	sendErr       error
	notifyCount   int
	notifyChannel string
	notifyPayload string
}

type fakeUpdatesStore struct {
	*fakeStore

	offset         int64
	offsetErr      error
	pollGeneration store.BotPollGeneration
	events         []store.Event
	messages       map[int64]store.Message
	users          map[int64]store.User
	lastFromPts    int64
	lastLimit      int
	messageLookups int
	senderLookups  int
}

type botAPIGetUpdatesStoreContract interface {
	BotUpdateOffset(context.Context, int64, string) (int64, error)
	StartBotUpdatePoll(context.Context, int64, string) (store.BotUpdatePoll, error)
	CheckBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration) error
	FinishBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration, int64) (int64, error)
	BotMessageEvents(context.Context, int64, int64, int) (store.BotMessageEventPage, error)
}

var _ botAPIGetUpdatesStoreContract = (botapi.Store)(nil)

type firstQueryUpdatesStore struct {
	*fakeUpdatesStore

	firstQuery chan struct{}
	queryCount atomic.Int32
}

func (s *firstQueryUpdatesStore) BotMessageEvents(ctx context.Context, botID, fromPts int64, limit int) (store.BotMessageEventPage, error) {
	if s.queryCount.Add(1) == 1 {
		close(s.firstQuery)
		return store.BotMessageEventPage{NextOffset: fromPts}, nil
	}
	return s.fakeUpdatesStore.BotMessageEvents(ctx, botID, fromPts, limit)
}

type blockingFirstQueryUpdatesStore struct {
	*fakeUpdatesStore

	firstQuery chan struct{}
	queryCount atomic.Int32
}

type deadlineAfterFirstPageUpdatesStore struct {
	*fakeUpdatesStore

	queryCount atomic.Int32
}

type sharedReplicaUpdatesStore struct {
	*fakeUpdatesStore

	mu          sync.Mutex
	generation  store.BotPollGeneration
	offset      int64
	events      []store.BotMessageEvent
	querySignal chan struct{}
	wakePolls   []func(int64)
}

type delayedFirstClaimUpdatesStore struct {
	*sharedReplicaUpdatesStore

	firstClaimStarted   chan struct{}
	releaseFirstClaim   chan struct{}
	secondClaimFinished chan struct{}
	startCalls          atomic.Int32
}

func (s *sharedReplicaUpdatesStore) BotUpdateOffset(_ context.Context, botID int64, token string) (int64, error) {
	if botID != 123456 || s.lookupErrs[token] != nil {
		return 0, store.ErrBotTokenNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offset, nil
}

func (s *sharedReplicaUpdatesStore) BotMessageEvents(_ context.Context, botID, fromPts int64, limit int) (store.BotMessageEventPage, error) {
	if botID != 123456 {
		return store.BotMessageEventPage{}, store.ErrBotTokenNotFound
	}
	s.querySignal <- struct{}{}
	s.mu.Lock()
	defer s.mu.Unlock()
	page := store.BotMessageEventPage{NextOffset: fromPts}
	for _, event := range s.events {
		if event.Pts < fromPts {
			continue
		}
		if len(page.Events) == limit {
			break
		}
		page.Events = append(page.Events, event)
		page.NextOffset = event.Pts + 1
	}
	return page, nil
}

func (s *sharedReplicaUpdatesStore) StartBotUpdatePoll(_ context.Context, botID int64, token string) (store.BotUpdatePoll, error) {
	if botID != 123456 || s.lookupErrs[token] != nil {
		return store.BotUpdatePoll{}, store.ErrBotTokenNotFound
	}
	s.mu.Lock()
	s.generation++
	generation, offset := s.generation, s.offset
	wakePolls := append([]func(int64){}, s.wakePolls...)
	s.mu.Unlock()
	for _, wake := range wakePolls {
		wake(botID)
	}
	return store.BotUpdatePoll{Offset: offset, Generation: generation}, nil
}

func (s *delayedFirstClaimUpdatesStore) StartBotUpdatePoll(ctx context.Context, botID int64, token string) (store.BotUpdatePoll, error) {
	call := s.startCalls.Add(1)
	if call == 1 {
		close(s.firstClaimStarted)
		<-s.releaseFirstClaim
		if err := ctx.Err(); err != nil {
			return store.BotUpdatePoll{}, err
		}
	}
	poll, err := s.sharedReplicaUpdatesStore.StartBotUpdatePoll(ctx, botID, token)
	if call == 2 && err == nil {
		close(s.secondClaimFinished)
	}
	return poll, err
}

func (s *sharedReplicaUpdatesStore) CheckBotUpdatePoll(_ context.Context, _ int64, _ string, generation store.BotPollGeneration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return store.ErrBotPollSuperseded
	}
	return nil
}

func (s *sharedReplicaUpdatesStore) FinishBotUpdatePoll(_ context.Context, botID int64, token string, generation store.BotPollGeneration, offset int64) (int64, error) {
	if botID != 123456 || s.lookupErrs[token] != nil {
		return 0, store.ErrBotTokenNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation {
		return 0, store.ErrBotPollSuperseded
	}
	if offset > s.offset {
		s.offset = offset
	}
	return s.offset, nil
}

func (s *blockingFirstQueryUpdatesStore) BotMessageEvents(ctx context.Context, botID, fromPts int64, limit int) (store.BotMessageEventPage, error) {
	if s.queryCount.Add(1) == 1 {
		close(s.firstQuery)
		<-ctx.Done()
		return store.BotMessageEventPage{}, ctx.Err()
	}
	return s.fakeUpdatesStore.BotMessageEvents(ctx, botID, fromPts, limit)
}

func (s *deadlineAfterFirstPageUpdatesStore) BotMessageEvents(ctx context.Context, botID, fromPts int64, limit int) (store.BotMessageEventPage, error) {
	if s.queryCount.Add(1) == 2 {
		<-ctx.Done()
		return store.BotMessageEventPage{}, ctx.Err()
	}
	return s.fakeUpdatesStore.BotMessageEvents(ctx, botID, fromPts, limit)
}

func (s *fakeUpdatesStore) BotUpdateOffset(_ context.Context, botID int64, token string) (int64, error) {
	if s.offsetErr != nil {
		return 0, s.offsetErr
	}
	if botID != 123456 || s.lookupErrs[token] != nil {
		return 0, store.ErrBotTokenNotFound
	}
	return s.offset, nil
}

func (s *fakeUpdatesStore) StartBotUpdatePoll(_ context.Context, botID int64, token string) (store.BotUpdatePoll, error) {
	if botID != 123456 || s.lookupErrs[token] != nil {
		return store.BotUpdatePoll{}, store.ErrBotTokenNotFound
	}
	s.pollGeneration++
	return store.BotUpdatePoll{Offset: s.offset, Generation: s.pollGeneration}, nil
}

func (s *fakeUpdatesStore) CheckBotUpdatePoll(_ context.Context, botID int64, token string, generation store.BotPollGeneration) error {
	if botID != 123456 || s.lookupErrs[token] != nil {
		return store.ErrBotTokenNotFound
	}
	if generation != s.pollGeneration {
		return store.ErrBotPollSuperseded
	}
	return nil
}

func (s *fakeUpdatesStore) FinishBotUpdatePoll(_ context.Context, botID int64, token string, generation store.BotPollGeneration, offset int64) (int64, error) {
	if err := s.CheckBotUpdatePoll(context.Background(), botID, token, generation); err != nil {
		return 0, err
	}
	if offset > s.offset {
		s.offset = offset
	}
	return s.offset, nil
}

func (s *fakeUpdatesStore) BotMessageEvents(_ context.Context, botID, fromPts int64, limit int) (store.BotMessageEventPage, error) {
	if botID != 123456 {
		return store.BotMessageEventPage{}, store.ErrBotTokenNotFound
	}
	s.lastFromPts = fromPts
	s.lastLimit = limit
	rawEvents := make([]store.Event, 0, limit)
	for _, event := range s.events {
		if int64(event.Pts) < fromPts {
			continue
		}
		if len(rawEvents) == limit {
			break
		}
		rawEvents = append(rawEvents, event)
	}
	page := store.BotMessageEventPage{NextOffset: fromPts}
	if len(rawEvents) != 0 {
		page.NextOffset = int64(rawEvents[len(rawEvents)-1].Pts) + 1
	}
	for _, event := range rawEvents {
		if event.Type != store.EventNewMessage {
			continue
		}
		message, exists := s.messages[event.LocalID]
		if !exists || message.OwnerID != botID || message.PeerType != store.PeerTypeUser || message.PeerID != message.FromID || message.FromID <= 0 || message.Out || message.Deleted || message.Action != 0 || message.Text == "" || message.FileID != 0 || message.StickerDocumentID != nil {
			continue
		}
		sender, exists := s.users[message.FromID]
		if !exists || sender.Kind != store.UserKindHuman {
			continue
		}
		page.Events = append(page.Events, store.BotMessageEvent{Pts: int64(event.Pts), Type: event.Type, LocalID: event.LocalID, Message: message, Sender: sender})
	}
	return page, nil
}

func (s *fakeUpdatesStore) MessageByOwnerLocal(_ context.Context, ownerID, localID int64) (store.Message, bool, error) {
	s.messageLookups++
	message, ok := s.messages[localID]
	return message, ok && message.OwnerID == ownerID, nil
}

func (s *fakeUpdatesStore) UserByID(ctx context.Context, id int64) (store.User, bool, error) {
	if id == 123456 {
		return s.fakeStore.UserByID(ctx, id)
	}
	s.senderLookups++
	user, ok := s.users[id]
	return user, ok, nil
}

type blockingLookupStore struct {
	lookupStarted chan struct{}
	lookupRelease chan struct{}
	lookupCount   atomic.Int32
}

func (s *blockingLookupStore) BotIDByToken(context.Context, string) (int64, error) {
	s.lookupCount.Add(1)
	s.lookupStarted <- struct{}{}
	<-s.lookupRelease
	return 0, store.ErrBotTokenNotFound
}

func (*blockingLookupStore) UserByID(context.Context, int64) (store.User, bool, error) {
	return store.User{}, false, nil
}

func (*blockingLookupStore) CheckRateLimit(context.Context, int64, string, store.RateLimitConfig) (*store.RateLimitResult, error) {
	return &store.RateLimitResult{}, nil
}

func (*blockingLookupStore) SendBotMessage(context.Context, int64, int64, string, int64) (store.Message, int, int, error) {
	return store.Message{}, 0, 0, nil
}

func (*blockingLookupStore) Notify(context.Context, string, string) error {
	return nil
}

func (*blockingLookupStore) BotUpdateOffset(context.Context, int64, string) (int64, error) {
	return 0, store.ErrBotTokenNotFound
}

func (*blockingLookupStore) StartBotUpdatePoll(context.Context, int64, string) (store.BotUpdatePoll, error) {
	return store.BotUpdatePoll{}, store.ErrBotTokenNotFound
}

func (*blockingLookupStore) CheckBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration) error {
	return store.ErrBotTokenNotFound
}

func (*blockingLookupStore) FinishBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration, int64) (int64, error) {
	return 0, store.ErrBotTokenNotFound
}

func (*blockingLookupStore) BotMessageEvents(context.Context, int64, int64, int) (store.BotMessageEventPage, error) {
	return store.BotMessageEventPage{}, store.ErrBotTokenNotFound
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

func (s *fakeStore) SendBotMessage(_ context.Context, botID, chatID int64, text string, randomID int64) (store.Message, int, int, error) {
	s.sendCount++
	s.sendBotID = botID
	s.sendChatID = chatID
	s.sendText = text
	s.sendRandom = randomID
	if s.sendErr != nil {
		return store.Message{}, 0, 0, s.sendErr
	}
	return store.Message{
		OwnerID: botID, LocalID: 7, PeerLocalID: 9, FromID: botID,
		Date: time.Unix(1700000000, 0), Text: text,
	}, 3, 4, nil
}

func (s *fakeStore) CheckRateLimit(_ context.Context, userID int64, surface string, config store.RateLimitConfig) (*store.RateLimitResult, error) {
	s.limitCount++
	s.limitUserID = userID
	s.limitSurface = surface
	s.limitConfig = config
	return s.limitResult, s.limitErr
}

func (s *fakeStore) Notify(_ context.Context, channel, payload string) error {
	s.notifyCount++
	s.notifyChannel = channel
	s.notifyPayload = payload
	return nil
}

func (*fakeStore) BotUpdateOffset(context.Context, int64, string) (int64, error) {
	return 0, store.ErrBotTokenNotFound
}

func (*fakeStore) StartBotUpdatePoll(context.Context, int64, string) (store.BotUpdatePoll, error) {
	return store.BotUpdatePoll{}, store.ErrBotTokenNotFound
}

func (*fakeStore) CheckBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration) error {
	return store.ErrBotTokenNotFound
}

func (*fakeStore) FinishBotUpdatePoll(context.Context, int64, string, store.BotPollGeneration, int64) (int64, error) {
	return 0, store.ErrBotTokenNotFound
}

func (*fakeStore) BotMessageEvents(context.Context, int64, int64, int) (store.BotMessageEventPage, error) {
	return store.BotMessageEventPage{}, store.ErrBotTokenNotFound
}

func newFakeStore() *fakeStore {
	username := "weather_bot"
	return &fakeStore{
		user:       store.User{ID: 123456, Kind: store.UserKindBot, FirstName: "Weather", Username: &username},
		userExists: true,
	}
}

func TestGetMeReturnsServerLookedUpBot(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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

func TestSendMessageReturnsBotMessageUsingTokenIdentity(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bot999999:secret-sentinel/sendMessage", strings.NewReader(`{"chat_id":42,"text":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %q", response.Code, response.Body.String())
	}
	const want = `{"ok":true,"result":{"message_id":9,"from":{"id":123456,"is_bot":true,"first_name":"Weather","username":"weather_bot"},"chat":{"id":42,"type":"private"},"date":1700000000,"text":"hi"}}` + "\n"
	if got := response.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if st.sendCount != 1 || st.sendBotID != 123456 || st.sendChatID != 42 || st.sendText != "hi" || st.sendRandom == 0 {
		t.Fatalf("send call = count:%d bot:%d chat:%d text:%q random_id:%d", st.sendCount, st.sendBotID, st.sendChatID, st.sendText, st.sendRandom)
	}
	if st.notifyCount != 1 || st.notifyChannel != store.ChannelUpdates || st.notifyPayload != "42" {
		t.Fatalf("update notification = count:%d channel:%q payload:%q", st.notifyCount, st.notifyChannel, st.notifyPayload)
	}
}

func TestSendMessageRejectsInvalidRequestTextAndChatID(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "string chat id", contentType: "application/json", body: `{"chat_id":"42","text":"hi"}`},
		{name: "null chat id", contentType: "application/json", body: `{"chat_id":null,"text":"hi"}`},
		{name: "fractional chat id", contentType: "application/json", body: `{"chat_id":42.5,"text":"hi"}`},
		{name: "missing chat id", contentType: "application/json", body: `{"text":"hi"}`},
		{name: "missing text", contentType: "application/json", body: `{"chat_id":42}`},
		{name: "empty text", contentType: "application/json", body: `{"chat_id":42,"text":""}`},
		{name: "NUL text", contentType: "application/json", body: `{"chat_id":42,"text":"\u0000"}`},
		{name: "too many UTF-16 units", contentType: "application/json", body: `{"chat_id":42,"text":"` + strings.Repeat("😀", 2049) + `"}`},
		{name: "too many basic characters", contentType: "application/json", body: `{"chat_id":42,"text":"` + strings.Repeat("x", 4097) + `"}`},
		{name: "username form chat id", contentType: "application/x-www-form-urlencoded", body: "chat_id=%40weather_bot&text=hi"},
		{name: "fractional form chat id", contentType: "application/x-www-form-urlencoded", body: "chat_id=42.5&text=hi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bot123456:secret-sentinel/sendMessage", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", tt.contentType)
			request.RemoteAddr = "100.64.1.2:3456"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %q", response.Code, response.Body.String())
			}
		})
	}
	if st.sendCount != 0 {
		t.Fatalf("invalid requests reached SendBotMessage %d times", st.sendCount)
	}
}

func TestSendMessageAcceptsQueryAndFormParameters(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
	}{
		{name: "query", method: http.MethodGet, path: "/bot123456:secret-sentinel/sendMessage?chat_id=42&text=hello"},
		{name: "form", method: http.MethodPost, path: "/bot123456:secret-sentinel/sendMessage", contentType: "application/x-www-form-urlencoded", body: "chat_id=42&text=hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
			request := httptest.NewRequestWithContext(context.Background(), tt.method, tt.path, strings.NewReader(tt.body))
			if tt.contentType != "" {
				request.Header.Set("Content-Type", tt.contentType)
			}
			request.RemoteAddr = "100.64.1.2:3456"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || st.sendChatID != 42 || st.sendText != "hello" {
				t.Fatalf("response = (%d, %q), sent chat:%d text:%q", response.Code, response.Body.String(), st.sendChatID, st.sendText)
			}
		})
	}
}

func TestSendMessageAcceptsMaximumUTF16Length(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	text := strings.Repeat("😀", 2048)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bot123456:secret-sentinel/sendMessage", strings.NewReader(`{"chat_id":42,"text":"`+text+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for exactly 4096 UTF-16 units; body %q", response.Code, response.Body.String())
	}
}

func TestSendMessageRateLimitUsesBotBudgetAndReturnsRetryAfter(t *testing.T) {
	st := newFakeStore()
	st.limitResult = &store.RateLimitResult{Wait: 1500 * time.Millisecond}
	rateLimit := store.RateLimitConfig{Limit: 2, Window: time.Minute}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), rateLimit)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bot123456:secret-sentinel/sendMessage", strings.NewReader(`{"chat_id":42,"text":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	want := "{\"ok\":false,\"error_code\":429,\"description\":\"Too Many Requests: retry after 2\",\"parameters\":{\"retry_after\":2}}\n"
	if response.Code != http.StatusTooManyRequests || response.Body.String() != want {
		t.Fatalf("response = (%d, %q), want 429 %q", response.Code, response.Body.String(), want)
	}
	if st.limitCount != 1 || st.limitUserID != 123456 || st.limitSurface != "message_send" || st.limitConfig != rateLimit {
		t.Fatalf("rate limit call = count:%d user:%d surface:%q config:%+v", st.limitCount, st.limitUserID, st.limitSurface, st.limitConfig)
	}
	if st.sendCount != 0 {
		t.Fatalf("rate limited request called SendBotMessage %d times", st.sendCount)
	}
}

func TestSendMessageMapsRecipientRefusalsToDistinctBotAPI403s(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "not started",
			err:  store.ErrBotRecipientForbidden,
			want: "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot can't initiate conversation with a user\"}\n",
		},
		{
			name: "bot blocked user",
			err:  store.ErrYouBlockedUser,
			want: "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot can't initiate conversation with a user\"}\n",
		},
		{
			name: "user blocked bot",
			err:  store.ErrBotBlockedByUser,
			want: "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot was blocked by the user\"}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			st.sendErr = tt.err
			handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
			request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bot123456:secret-sentinel/sendMessage", strings.NewReader(`{"chat_id":42,"text":"hi"}`))
			request.Header.Set("Content-Type", "application/json")
			request.RemoteAddr = "100.64.1.2:3456"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || response.Body.String() != tt.want {
				t.Fatalf("response = (%d, %q), want 403 %q", response.Code, response.Body.String(), tt.want)
			}
		})
	}
}

func TestValidGetMeRequestsDoNotConsumeUnauthorizedRateLimit(t *testing.T) {
	st := newFakeStore()
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getWebhookInfo", nil)
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
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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

func TestConcurrentUnauthorizedRequestsAreCappedBeforeTokenLookup(t *testing.T) {
	const requestCount = authAttemptLimit + 1
	st := &blockingLookupStore{
		lookupStarted: make(chan struct{}, requestCount),
		lookupRelease: make(chan struct{}),
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	start := make(chan struct{})
	results := make(chan int, requestCount)
	for range requestCount {
		go func() {
			<-start
			request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/botbad-token/getMe", nil)
			request.RemoteAddr = "198.51.100.4:3456"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			results <- response.Code
		}()
	}
	close(start)

	var releaseOnce sync.Once
	releaseLookups := func() {
		releaseOnce.Do(func() { close(st.lookupRelease) })
	}
	defer releaseLookups()

	for range authAttemptLimit {
		select {
		case <-st.lookupStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("did not reach the expected concurrent token lookups")
		}
	}
	select {
	case status := <-results:
		if status != http.StatusTooManyRequests {
			t.Fatalf("request outside the auth budget returned %d, want 429", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request beyond the auth budget reached token lookup instead of returning 429")
	}

	releaseLookups()
	for range authAttemptLimit {
		select {
		case status := <-results:
			if status != http.StatusUnauthorized {
				t.Fatalf("bad-token request returned %d, want 401", status)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("admitted token lookup did not finish")
		}
	}
	if got := st.lookupCount.Load(); got != authAttemptLimit {
		t.Fatalf("concurrent token lookups = %d, want %d", got, authAttemptLimit)
	}
}

func TestUnauthorizedRateLimitIsSharedBySourceNetwork(t *testing.T) {
	st := newFakeStore()
	st.lookupErrs = map[string]error{"bad-token": store.ErrBotTokenNotFound}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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
	server := botapi.NewServer("127.0.0.1:0", st, logger, store.RateLimitConfig{})
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
	botapi.NewServer("127.0.0.1:0", st, logger, store.RateLimitConfig{}).ErrorLog.Printf("request /bot%s/getMe failed", token)

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
	server := botapi.NewServer("127.0.0.1:0", newFakeStore(), slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	if server.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want positive", server.ReadHeaderTimeout)
	}
	if server.MaxHeaderBytes <= 0 {
		t.Errorf("MaxHeaderBytes = %d, want positive", server.MaxHeaderBytes)
	}
	if server.ErrorLog == nil {
		t.Error("ErrorLog is nil")
	}
	if server.WriteTimeout <= 50*time.Second {
		t.Errorf("WriteTimeout = %v, want more than the maximum getUpdates poll", server.WriteTimeout)
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
	handler := botapi.NewHandler(newFakeStore(), slog.New(slog.DiscardHandler), store.RateLimitConfig{})
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

func TestGetUpdatesLogsAllowedUpdatesParseError(t *testing.T) {
	st := &fakeUpdatesStore{fakeStore: newFakeStore()}
	var logs bytes.Buffer
	handler := botapi.NewHandler(st, slog.New(slog.NewTextHandler(&logs, nil)), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/bot123456:secret-sentinel/getUpdates?allowed_updates=%5B%22message%22%2C%22callback_query%22%5D", nil)
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(logs.String(), "getUpdates only supports message updates") {
		t.Fatalf("request log = %q, want the allowed_updates parse reason", logs.String())
	}
}

func TestGetUpdatesLogsOffsetLookupError(t *testing.T) {
	lookupErr := errors.New("database unavailable")
	st := &fakeUpdatesStore{fakeStore: newFakeStore(), offsetErr: lookupErr}
	var logs bytes.Buffer
	handler := botapi.NewHandler(st, slog.New(slog.NewTextHandler(&logs, nil)), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/bot123456:secret-sentinel/getUpdates?timeout=0", nil)
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if !strings.Contains(logs.String(), lookupErr.Error()) {
		t.Fatalf("request log = %q, want offset lookup cause", logs.String())
	}
}

func TestGetUpdatesReturnsOnlyIncomingPrivateTextWithExactFields(t *testing.T) {
	st := &fakeUpdatesStore{
		fakeStore: newFakeStore(),
		events:    []store.Event{{Pts: 7, Type: store.EventNewMessage, LocalID: 42}},
		messages: map[int64]store.Message{
			42: {
				OwnerID: 123456, LocalID: 42, PeerType: store.PeerTypeUser, PeerID: 987,
				FromID: 987, Date: time.Unix(1700000000, 0), Text: "hello", FileID: 0,
			},
		},
		users: map[int64]store.User{
			987: {ID: 987, Kind: store.UserKindHuman, FirstName: "Ada", LastName: "Lovelace", Username: new("ada")},
		},
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/bot123456:secret-sentinel/getUpdates", strings.NewReader(`{"offset":0,"limit":100,"timeout":0}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	const want = `{"ok":true,"result":[{"update_id":7,"message":{"message_id":42,"from":{"id":987,"first_name":"Ada","last_name":"Lovelace","username":"ada"},"chat":{"id":987,"first_name":"Ada","last_name":"Lovelace","username":"ada","type":"private"},"date":1700000000,"text":"hello"}}]}` + "\n"
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("getUpdates response = (%d, %q), want 200 %q", response.Code, response.Body.String(), want)
	}
	if st.messageLookups != 0 || st.senderLookups != 0 {
		t.Fatalf("getUpdates performed %d message and %d sender lookups, want a single joined event query", st.messageLookups, st.senderLookups)
	}
}

func TestGetUpdatesUsesStoredOffsetWhenClientReplaysAnOlderOffset(t *testing.T) {
	st := &fakeUpdatesStore{
		fakeStore: newFakeStore(),
		offset:    9,
		events:    []store.Event{{Pts: 8, Type: store.EventNewMessage, LocalID: 1}},
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/bot123456:secret-sentinel/getUpdates", strings.NewReader(`{"offset":2,"timeout":0}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"ok\":true,\"result\":[]}\n" {
		t.Fatalf("getUpdates response = (%d, %q), want empty 200", response.Code, response.Body.String())
	}
	if st.lastFromPts != 9 || st.offset != 9 {
		t.Fatalf("effective offset = %d, stored offset = %d, want both 9", st.lastFromPts, st.offset)
	}
}

func TestGetUpdatesWakeupReReadsTheBotLog(t *testing.T) {
	st := &firstQueryUpdatesStore{
		fakeUpdatesStore: &fakeUpdatesStore{
			fakeStore: newFakeStore(),
			messages: map[int64]store.Message{
				42: {OwnerID: 123456, LocalID: 42, PeerType: store.PeerTypeUser, PeerID: 987, FromID: 987, Date: time.Unix(1700000000, 0), Text: "hello"},
			},
			users: map[int64]store.User{
				987: {ID: 987, Kind: store.UserKindHuman, FirstName: "Ada"},
			},
		},
		firstQuery: make(chan struct{}),
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?timeout=5", nil)
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-st.firstQuery:
	case <-time.After(time.Second):
		t.Fatal("getUpdates did not read the log")
	}
	st.events = []store.Event{{Pts: 7, Type: store.EventNewMessage, LocalID: 42}}
	handler.NotifyUpdates(123456)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("getUpdates did not wake after an update notification")
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"update_id":7`) {
		t.Fatalf("getUpdates response = (%d, %q), want update 7", response.Code, response.Body.String())
	}
}

func TestGetUpdatesSecondPollConflictsTheFirst(t *testing.T) {
	st := &blockingFirstQueryUpdatesStore{
		fakeUpdatesStore: &fakeUpdatesStore{fakeStore: newFakeStore()},
		firstQuery:       make(chan struct{}),
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	firstRequest := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?timeout=5", nil)
	firstRequest.RemoteAddr = "100.64.1.2:3456"
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	select {
	case <-st.firstQuery:
	case <-time.After(time.Second):
		t.Fatal("first getUpdates did not begin reading")
	}
	secondRequest := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates", nil)
	secondRequest.RemoteAddr = "100.64.1.2:3456"
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first getUpdates was not canceled by the second poll")
	}
	if firstResponse.Code != http.StatusConflict || firstResponse.Body.String() != "{\"ok\":false,\"error_code\":409,\"description\":\"Conflict: terminated by other getUpdates request\"}\n" {
		t.Fatalf("first poll response = (%d, %q), want 409 conflict", firstResponse.Code, firstResponse.Body.String())
	}
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("second poll response = (%d, %q), want 200", secondResponse.Code, secondResponse.Body.String())
	}
}

func TestGetUpdatesAdvancesStoredOffsetAcrossPhotoGaps(t *testing.T) {
	st := &fakeUpdatesStore{
		fakeStore: newFakeStore(),
		events: []store.Event{
			{Pts: 1, Type: store.EventNewMessage, LocalID: 10},
			{Pts: 2, Type: store.EventNewMessage, LocalID: 11},
		},
		messages: map[int64]store.Message{
			10: {OwnerID: 123456, LocalID: 10, PeerType: store.PeerTypeUser, PeerID: 987, FromID: 987, Text: "photo", FileID: 42},
			11: {OwnerID: 123456, LocalID: 11, PeerType: store.PeerTypeUser, PeerID: 987, FromID: 987, Text: "photo", FileID: 42},
		},
		users: map[int64]store.User{
			987: {ID: 987, Kind: store.UserKindHuman, FirstName: "Ada"},
		},
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?offset=0", nil)
	request.RemoteAddr = "100.64.1.2:3456"
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, request)
	if firstResponse.Code != http.StatusOK || !strings.Contains(firstResponse.Body.String(), `"result":[]`) {
		t.Fatalf("photo-only poll response = (%d, %q), want empty success", firstResponse.Code, firstResponse.Body.String())
	}
	if st.offset != 3 {
		t.Fatalf("offset after photo-only poll = %d, want to skip proven pts gaps through 2", st.offset)
	}

	st.events = append(st.events, store.Event{Pts: 3, Type: store.EventNewMessage, LocalID: 12})
	st.messages[12] = store.Message{
		OwnerID: 123456, LocalID: 12, PeerType: store.PeerTypeUser, PeerID: 987,
		FromID: 987, Date: time.Unix(1700000000, 0), Text: "after photos",
	}
	secondRequest := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?offset=0", nil)
	secondRequest.RemoteAddr = "100.64.1.2:3457"
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if st.lastFromPts != 4 {
		t.Fatalf("second poll stopped at pts %d, want the exhausted page after update 3", st.lastFromPts)
	}
	if secondResponse.Code != http.StatusOK || !strings.Contains(secondResponse.Body.String(), `"update_id":3`) {
		t.Fatalf("poll after photo history = (%d, %q), want update 3", secondResponse.Code, secondResponse.Body.String())
	}
	if st.offset != 3 {
		t.Fatalf("offset after returning the unacknowledged update = %d, want 3", st.offset)
	}
}

func TestGetUpdatesScansPastMoreThanOnePageOfSkippedEvents(t *testing.T) {
	events := make([]store.Event, 101)
	messages := make(map[int64]store.Message, len(events))
	for i := range events {
		pts := i + 1
		events[i] = store.Event{Pts: pts, Type: store.EventNewMessage, LocalID: int64(pts)}
		messages[int64(pts)] = store.Message{
			OwnerID: 123456, LocalID: int64(pts), PeerType: store.PeerTypeUser,
			PeerID: 987, FromID: 987, Text: "photo", FileID: 42,
		}
	}
	messages[101] = store.Message{
		OwnerID: 123456, LocalID: 101, PeerType: store.PeerTypeUser,
		PeerID: 987, FromID: 987, Date: time.Unix(1700000000, 0), Text: "after gaps",
	}
	st := &fakeUpdatesStore{
		fakeStore: newFakeStore(),
		events:    events,
		messages:  messages,
		users: map[int64]store.User{
			987: {ID: 987, Kind: store.UserKindHuman, FirstName: "Ada"},
		},
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/bot123456:secret-sentinel/getUpdates?offset=0&limit=100&timeout=0", nil)
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"update_id":101`) || !strings.Contains(response.Body.String(), `"text":"after gaps"`) {
		t.Fatalf("getUpdates response = (%d, %q), want update 101 after the skipped page", response.Code, response.Body.String())
	}
	if st.lastFromPts != 102 {
		t.Fatalf("scan stopped at pts %d, want the exhausted page after 101", st.lastFromPts)
	}
	if st.offset != 101 {
		t.Fatalf("stored offset = %d, want returned update's inclusive offset 101", st.offset)
	}
}

func TestGetUpdatesReturnsWithinTimeoutAndStoresCompletedPageProgress(t *testing.T) {
	const pageLimit = 100
	events := make([]store.Event, pageLimit)
	for i := range events {
		events[i] = store.Event{Pts: i + 1, Type: store.EventNewMessage, LocalID: int64(i + 1)}
	}
	st := &deadlineAfterFirstPageUpdatesStore{
		fakeUpdatesStore: &fakeUpdatesStore{fakeStore: newFakeStore(), events: events},
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/bot123456:secret-sentinel/getUpdates?offset=0&limit=100&timeout=1", nil)
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, request)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timed getUpdates took %s, want it to stop at its one-second poll deadline", elapsed)
	}
	if response.Code != http.StatusOK || response.Body.String() != "{\"ok\":true,\"result\":[]}\n" {
		t.Fatalf("timed getUpdates response = (%d, %q), want empty success", response.Code, response.Body.String())
	}
	if st.queryCount.Load() != 2 || st.offset != pageLimit+1 {
		t.Fatalf("timed scan pages/offset = %d/%d, want 2 pages and completed offset %d", st.queryCount.Load(), st.offset, pageLimit+1)
	}
}

func TestGetUpdatesAcrossHandlerInstancesSupersedesOlderPoll(t *testing.T) {
	shared := &sharedReplicaUpdatesStore{
		fakeUpdatesStore: &fakeUpdatesStore{fakeStore: newFakeStore()},
		querySignal:      make(chan struct{}, 8),
		events:           nil,
	}
	firstHandler := botapi.NewHandler(shared, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	secondHandler := botapi.NewHandler(shared, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	shared.wakePolls = []func(int64){firstHandler.NotifyUpdates, secondHandler.NotifyUpdates}

	firstRequest := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?timeout=5", nil)
	firstRequest.RemoteAddr = "100.64.1.2:3456"
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		firstHandler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	select {
	case <-shared.querySignal:
	case <-time.After(time.Second):
		t.Fatal("first getUpdates did not read the log")
	}

	secondRequest := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?offset=3&timeout=5", nil)
	secondRequest.RemoteAddr = "100.64.1.2:3457"
	secondResponse := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		secondHandler.ServeHTTP(secondResponse, secondRequest)
		close(secondDone)
	}()
	select {
	case <-shared.querySignal:
	case <-time.After(time.Second):
		t.Fatal("second getUpdates did not read the log")
	}

	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("superseded getUpdates did not finish")
	}

	shared.mu.Lock()
	shared.events = []store.BotMessageEvent{{
		Pts: 4, Type: store.EventNewMessage, LocalID: 42,
		Message: store.Message{
			OwnerID: 123456, LocalID: 42, PeerType: store.PeerTypeUser, PeerID: 987,
			FromID: 987, Date: time.Unix(1700000000, 0), Text: "replica update",
		},
		Sender: store.User{ID: 987, Kind: store.UserKindHuman, FirstName: "Ada"},
	}}
	wakePolls := append([]func(int64){}, shared.wakePolls...)
	shared.mu.Unlock()
	for _, wake := range wakePolls {
		wake(123456)
	}

	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("current getUpdates did not finish after update wakeup")
	}
	if firstResponse.Code != http.StatusConflict {
		t.Fatalf("first poll response = (%d, %q), want 409 conflict", firstResponse.Code, firstResponse.Body.String())
	}
	if secondResponse.Code != http.StatusOK || !strings.Contains(secondResponse.Body.String(), `"update_id":4`) {
		t.Fatalf("second poll response = (%d, %q), want update 4", secondResponse.Code, secondResponse.Body.String())
	}
	shared.mu.Lock()
	offset := shared.offset
	shared.mu.Unlock()
	if offset != 4 {
		t.Fatalf("stored offset = %d, want the winning poll's safe gap offset 4", offset)
	}
}

func TestGetUpdatesSupersededClaimCannotFinishAfterNewerClaim(t *testing.T) {
	st := &delayedFirstClaimUpdatesStore{
		sharedReplicaUpdatesStore: &sharedReplicaUpdatesStore{
			fakeUpdatesStore: &fakeUpdatesStore{fakeStore: newFakeStore()},
			querySignal:      make(chan struct{}, 8),
		},
		firstClaimStarted:   make(chan struct{}),
		releaseFirstClaim:   make(chan struct{}),
		secondClaimFinished: make(chan struct{}),
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	st.wakePolls = []func(int64){handler.NotifyUpdates}

	firstRequest := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?timeout=5", nil)
	firstRequest.RemoteAddr = "100.64.1.2:3456"
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	select {
	case <-st.firstClaimStarted:
	case <-time.After(time.Second):
		t.Fatal("first getUpdates did not begin claiming a generation")
	}

	secondRequest := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?offset=5&timeout=5", nil)
	secondRequest.RemoteAddr = "100.64.1.2:3457"
	secondResponse := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(secondResponse, secondRequest)
		close(secondDone)
	}()
	select {
	case <-st.secondClaimFinished:
	case <-time.After(time.Second):
		t.Fatal("newer getUpdates did not finish claiming its generation")
	}

	close(st.releaseFirstClaim)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("superseded getUpdates did not finish after its claim was released")
	}
	if firstResponse.Code != http.StatusConflict {
		t.Fatalf("superseded poll response = (%d, %q), want 409 conflict", firstResponse.Code, firstResponse.Body.String())
	}

	st.mu.Lock()
	st.events = []store.BotMessageEvent{{
		Pts: 6, Type: store.EventNewMessage, LocalID: 42,
		Message: store.Message{
			OwnerID: 123456, LocalID: 42, PeerType: store.PeerTypeUser, PeerID: 987,
			FromID: 987, Date: time.Unix(1700000000, 0), Text: "newer poll update",
		},
		Sender: store.User{ID: 987, Kind: store.UserKindHuman, FirstName: "Ada"},
	}}
	st.mu.Unlock()
	handler.NotifyUpdates(123456)
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("newer getUpdates did not finish after the update notification")
	}
	if secondResponse.Code != http.StatusOK || !strings.Contains(secondResponse.Body.String(), `"update_id":6`) {
		t.Fatalf("newer poll response = (%d, %q), want update 6", secondResponse.Code, secondResponse.Body.String())
	}
	st.mu.Lock()
	offset, generation := st.offset, st.generation
	st.mu.Unlock()
	if offset != 6 || generation != 1 {
		t.Fatalf("stored offset/generation = %d/%d, want 6/1", offset, generation)
	}
}

func TestGetUpdatesRevocationBeforeWakeReturnsUnauthorizedWithoutOffsetAdvance(t *testing.T) {
	st := &firstQueryUpdatesStore{
		fakeUpdatesStore: &fakeUpdatesStore{
			fakeStore: newFakeStore(),
			offset:    2,
			messages: map[int64]store.Message{
				42: {OwnerID: 123456, LocalID: 42, PeerType: store.PeerTypeUser, PeerID: 987, FromID: 987, Date: time.Unix(1700000000, 0), Text: "revoked"},
			},
			users: map[int64]store.User{
				987: {ID: 987, Kind: store.UserKindHuman, FirstName: "Ada"},
			},
		},
		firstQuery: make(chan struct{}),
	}
	handler := botapi.NewHandler(st, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123456:secret-sentinel/getUpdates?offset=10&timeout=5", nil)
	request.RemoteAddr = "100.64.1.2:3456"
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-st.firstQuery:
	case <-time.After(time.Second):
		t.Fatal("getUpdates did not read the log")
	}
	st.lookupErrs = map[string]error{"123456:secret-sentinel": store.ErrBotTokenNotFound}
	st.events = []store.Event{{Pts: 11, Type: store.EventNewMessage, LocalID: 42}}
	handler.NotifyUpdates(123456)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoked poll did not finish after wakeup")
	}
	if response.Code != http.StatusUnauthorized || response.Body.String() != "{\"ok\":false,\"error_code\":401,\"description\":\"Unauthorized\"}\n" {
		t.Fatalf("revoked poll response = (%d, %q), want 401", response.Code, response.Body.String())
	}
	if st.offset != 2 {
		t.Fatalf("offset after revoked poll = %d, want unchanged 2", st.offset)
	}
}
