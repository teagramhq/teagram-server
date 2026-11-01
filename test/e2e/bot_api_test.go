package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/botapi"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBotAPIGetMe(t *testing.T) {
	t.Parallel()
	testSmokeBotAPIGetMe(t)
}

func TestBotAPISendMessage(t *testing.T) {
	t.Parallel()
	testSmokeBotAPISendMessage(t)
}

func testSmokeBotAPIGetMe(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_weather_bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(f.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Bot API: %v", err)
	}
	listener = botapi.LimitConnectionsPerIP(listener, botapi.DefaultMaxConnectionsPerIP)
	server := botapi.NewServer(listener.Addr().String(), f.store, slog.New(slog.DiscardHandler), store.RateLimitConfig{})
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Bot API shutdown failed")
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Bot API server stopped unexpectedly")
		}
	})

	request, err := http.NewRequestWithContext(f.ctx, http.MethodGet,
		"http://"+listener.Addr().String()+"/bot"+token+"/getMe", nil)
	if err != nil {
		t.Fatal("build Bot API request")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("Bot API request failed")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close Bot API response: %v", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Bot API status = %d, want 200", response.StatusCode)
	}
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			ID        int64  `json:"id"`
			IsBot     bool   `json:"is_bot"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode Bot API response: %v", err)
	}
	if !got.OK || got.Result.ID != bot.ID || !got.Result.IsBot || got.Result.FirstName != "Weather" || got.Result.Username != "smoke_weather_bot" {
		t.Fatalf("getMe result = %+v, want bot profile id %d", got, bot.ID)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read Bot API response: %v", err)
	}
}

func testSmokeBotAPISendMessage(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_send_bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	otherBot, _, err := f.store.CreateBot(f.ctx, "smoke_other_bot", "Other")
	if err != nil {
		t.Fatalf("create other bot: %v", err)
	}
	const phoneStarted, phoneNever, phoneBlocked = "+15551049311", "+15551049312", "+15551049313"
	seedSmokeUsers(t, f, phoneStarted, phoneNever, phoneBlocked)
	neverStarted := smokeUserByPhone(t, f, phoneNever)
	started := newSmokeClient(t, f, "Bot API started user", phoneStarted)
	blocked := newSmokeClient(t, f, "Bot API blocked user", phoneBlocked)
	server := startSmokeBotAPIServer(t, f, store.RateLimitConfig{})
	client := &http.Client{Timeout: 5 * time.Second}

	if err := started.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(started.id, bot.ID), Message: "/start", RandomID: 2818001,
		})
		return err
	}); err != nil {
		t.Fatalf("send /start to bot: %v", err)
	}
	if err := blocked.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(blocked.id, bot.ID), Message: "/start", RandomID: 2818002,
		})
		return err
	}); err != nil {
		t.Fatalf("send /start before blocking bot: %v", err)
	}

	response := sendBotAPIMessage(t, f.ctx, client, server, token, started.id, "hi")
	if response.Code != http.StatusOK {
		t.Fatalf("sendMessage status = %d, want 200; body %q", response.Code, response.Body)
	}
	var sent struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
			From      struct {
				ID        int64  `json:"id"`
				IsBot     bool   `json:"is_bot"`
				FirstName string `json:"first_name"`
				Username  string `json:"username"`
			} `json:"from"`
			Chat struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
			Date int    `json:"date"`
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body, &sent); err != nil {
		t.Fatalf("decode sendMessage response: %v", err)
	}
	if !sent.OK || sent.Result.From.ID != bot.ID || !sent.Result.From.IsBot || sent.Result.From.FirstName != "Weather" || sent.Result.From.Username != "smoke_send_bot" || sent.Result.Chat.ID != started.id || sent.Result.Chat.Type != "private" || sent.Result.Text != "hi" || sent.Result.Date <= 0 {
		t.Fatalf("sendMessage result = %+v, want bot %d speaking to user %d", sent, bot.ID, started.id)
	}
	received := recvOrCtx(t, f.ctx, started.push.newMsg, "Bot API reply update")
	from, fromOK := received.FromID.(*tg.PeerUser)
	peer, peerOK := received.PeerID.(*tg.PeerUser)
	if received.ID != sent.Result.MessageID || !fromOK || from.UserID != bot.ID || received.Out || !peerOK || peer.UserID != bot.ID || received.Message != "hi" {
		t.Fatalf("human received message = %+v, want normal incoming message from bot %d", received, bot.ID)
	}

	forbidden := "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot can't initiate conversation with a user\"}\n"
	for label, targetID := range map[string]int64{
		"unknown user":  1 << 62,
		"never-started": neverStarted.ID,
		"another bot":   otherBot.ID,
	} {
		result := sendBotAPIMessage(t, f.ctx, client, server, token, targetID, "hi")
		if result.Code != http.StatusForbidden || string(result.Body) != forbidden {
			t.Errorf("%s response = (%d, %q), want identical 403 %q", label, result.Code, result.Body, forbidden)
		}
	}
	neverStartedHistory, err := f.store.History(f.ctx, neverStarted.ID, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read never-started user's history: %v", err)
	}
	if len(neverStartedHistory) != 0 {
		t.Fatalf("never-started user's history after refused send = %+v, want empty", neverStartedHistory)
	}

	if _, err := f.store.BlockUser(f.ctx, blocked.id, bot.ID); err != nil {
		t.Fatalf("block bot from user: %v", err)
	}
	blockedBefore, err := f.store.History(f.ctx, blocked.id, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read blocked user's history before send: %v", err)
	}
	blockedResult := sendBotAPIMessage(t, f.ctx, client, server, token, blocked.id, "blocked reply")
	blockedBody := "{\"ok\":false,\"error_code\":403,\"description\":\"Forbidden: bot was blocked by the user\"}\n"
	if blockedResult.Code != http.StatusForbidden || string(blockedResult.Body) != blockedBody {
		t.Fatalf("blocked send response = (%d, %q), want 403 %q", blockedResult.Code, blockedResult.Body, blockedBody)
	}
	blockedAfter, err := f.store.History(f.ctx, blocked.id, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read blocked user's history after send: %v", err)
	}
	if len(blockedAfter) != len(blockedBefore) {
		t.Fatalf("blocked user's history count = %d after send, want unchanged at %d", len(blockedAfter), len(blockedBefore))
	}
}

func TestBotAPISendMessageRateLimit(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	bot, token, err := f.store.CreateBot(f.ctx, "smoke_limit_bot", "Limit")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	const phone = "+15551049314"
	seedSmokeUsers(t, f, phone)
	human := newSmokeClient(t, f, "Bot API limited user", phone)
	if err := human.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(human.id, bot.ID), Message: "/start", RandomID: 2818003,
		})
		return err
	}); err != nil {
		t.Fatalf("send /start to bot: %v", err)
	}
	server := startSmokeBotAPIServer(t, f, store.RateLimitConfig{Limit: 2, Window: time.Hour})
	client := &http.Client{Timeout: 5 * time.Second}
	for range 2 {
		response := sendBotAPIMessage(t, f.ctx, client, server, token, human.id, "limited")
		if response.Code != http.StatusOK {
			t.Fatalf("sendMessage under limit status = %d, want 200; body %q", response.Code, response.Body)
		}
	}
	response := sendBotAPIMessage(t, f.ctx, client, server, token, human.id, "over limit")
	var limited struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(response.Body, &limited); err != nil {
		t.Fatalf("decode flood-wait response: %v", err)
	}
	wantDescription := "Too Many Requests: retry after " + strconv.Itoa(limited.Parameters.RetryAfter)
	if response.Code != http.StatusTooManyRequests || limited.OK || limited.ErrorCode != http.StatusTooManyRequests || limited.Parameters.RetryAfter < 1 || limited.Description != wantDescription {
		t.Fatalf("over-limit response = (%d, %+v), want a 429 with retry_after", response.Code, limited)
	}
	history, err := f.store.History(f.ctx, human.id, store.PeerTypeUser, bot.ID, 0, 10)
	if err != nil {
		t.Fatalf("read recipient history after flood wait: %v", err)
	}
	if len(history) != 3 || history[0].Text != "limited" || history[1].Text != "limited" || history[2].Text != "/start" {
		t.Fatalf("recipient history after flood wait = %+v, want two bot messages after /start", history)
	}
}

type botAPIHTTPResponse struct {
	Code int
	Body []byte
}

func startSmokeBotAPIServer(t *testing.T, f *smokeFixture, messageSendLimit store.RateLimitConfig) string {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(f.ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Bot API: %v", err)
	}
	server := botapi.NewServer(listener.Addr().String(), f.store, slog.New(slog.DiscardHandler), messageSendLimit)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Bot API shutdown failed")
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Bot API server stopped unexpectedly")
		}
	})
	return "http://" + listener.Addr().String()
}

func sendBotAPIMessage(t *testing.T, ctx context.Context, client *http.Client, serverURL, token string, chatID int64, text string) botAPIHTTPResponse {
	t.Helper()
	body, err := json.Marshal(struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: chatID, Text: text})
	if err != nil {
		t.Fatalf("encode Bot API sendMessage: %v", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/bot"+token+"/sendMessage", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build Bot API sendMessage request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Bot API sendMessage request failed: %v", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close Bot API response: %v", err)
		}
	}()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Bot API response: %v", err)
	}
	return botAPIHTTPResponse{Code: response.StatusCode, Body: responseBody}
}
