package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/botapi"
)

func TestBotAPIGetMe(t *testing.T) {
	t.Parallel()
	testSmokeBotAPIGetMe(t)
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
	server := botapi.NewServer(listener.Addr().String(), f.store, slog.New(slog.DiscardHandler))
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
