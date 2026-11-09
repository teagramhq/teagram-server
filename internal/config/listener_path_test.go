package config_test

import (
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestLoadRejectsTailnetPathForPublicWebSocket(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "127.0.0.1:2446")
	t.Setenv("TG_PUBLIC_WEBSOCKET_LISTEN_PATH", "tailnet")

	_, err := config.Load(discardLog())
	if err == nil {
		t.Fatal("Load succeeded with a tailnet public WebSocket path")
	}
	if !strings.Contains(err.Error(), "TG_PUBLIC_WEBSOCKET_LISTEN_PATH") {
		t.Fatalf("Load error = %q, want it to name TG_PUBLIC_WEBSOCKET_LISTEN_PATH", err)
	}
}
