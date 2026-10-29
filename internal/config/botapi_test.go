package config_test

import (
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestLoadBotAPIListenAddr(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		listen    string
		websocket string
		publicWS  string
		admin     string
		wantAddr  string
		wantError bool
	}{
		{name: "disabled"},
		{name: "loopback IPv4", addr: "127.0.0.1:2445", wantAddr: "127.0.0.1:2445"},
		{name: "loopback IPv6", addr: "[::1]:2445", wantAddr: "[::1]:2445"},
		{name: "tailnet IPv4", addr: "100.64.0.1:2445", wantAddr: "100.64.0.1:2445"},
		{name: "tailnet IPv6", addr: "[fd7a:115c:a1e0::1]:2445", wantAddr: "[fd7a:115c:a1e0::1]:2445"},
		{name: "empty host", addr: ":2445", wantError: true},
		{name: "IPv4 wildcard", addr: "0.0.0.0:2445", wantError: true},
		{name: "IPv6 wildcard", addr: "[::]:2445", wantError: true},
		{name: "hostname", addr: "localhost:2445", wantError: true},
		{name: "LAN address", addr: "192.168.1.8:2445", wantError: true},
		{name: "outside CGNAT", addr: "100.63.255.255:2445", wantError: true},
		{name: "outside tailnet prefix", addr: "[fd7a:115c:a1e1::1]:2445", wantError: true},
		{name: "invalid port", addr: "127.0.0.1:65536", wantError: true},
		{name: "conflicts with MTProto listener", addr: "127.0.0.1:2443", wantError: true},
		{name: "conflicts with wildcard MTProto listener", addr: "127.0.0.1:2446", listen: ":2446", wantError: true},
		{name: "conflicts with websocket listener", addr: "127.0.0.1:2447", websocket: "127.0.0.1:2447", wantError: true},
		{name: "conflicts with public websocket listener", addr: "100.64.0.8:2448", publicWS: "100.64.0.8:2448", wantError: true},
		{name: "conflicts with admin listener", addr: "127.0.0.1:2449", admin: "127.0.0.1:2449", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listen := tt.listen
			if listen == "" {
				listen = "127.0.0.1:2443"
			}
			t.Setenv("TG_LISTEN_ADDR", listen)
			t.Setenv("TG_WEBSOCKET_LISTEN_ADDR", tt.websocket)
			t.Setenv("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", tt.publicWS)
			t.Setenv("TG_ADMIN_LISTEN_ADDR", tt.admin)
			t.Setenv("TG_ADMIN_TOKEN_HASH", "")
			if tt.admin != "" {
				t.Setenv("TG_ADMIN_TOKEN_HASH", strings.Repeat("a", 64))
			}
			t.Setenv("TG_ADMIN_ORIGIN", "")
			t.Setenv("TG_BOT_API_LISTEN_ADDR", tt.addr)
			t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
			t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
			t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")

			cfg, err := config.Load(discardLog())
			if tt.wantError {
				if err == nil {
					t.Fatal("Load succeeded for a refused Bot API listener address")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BotAPIListenAddr != tt.wantAddr {
				t.Errorf("BotAPIListenAddr = %q, want %q", cfg.BotAPIListenAddr, tt.wantAddr)
			}
		})
	}
}
