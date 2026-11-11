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
			t.Setenv("TG_BOT_API_PUBLIC_LISTEN_ADDR", "")
			t.Setenv("TG_BOT_API_TAILNET_LISTEN_ADDR", "")
			t.Setenv("TG_BOT_API_PUBLIC_PROXY_CIDRS", "")
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

func TestLoadBotAPIPathListeners(t *testing.T) {
	tests := []struct {
		name        string
		legacy      string
		public      string
		tailnet     string
		proxies     string
		wantPublic  string
		wantTailnet string
		wantProxies []string
		wantError   bool
	}{
		{
			name:        "public and tailnet paths",
			public:      "0.0.0.0:2445",
			tailnet:     "100.64.0.8:2446",
			proxies:     "10.0.0.0/8,fd00::/8",
			wantPublic:  "0.0.0.0:2445",
			wantTailnet: "100.64.0.8:2446",
			wantProxies: []string{"10.0.0.0/8", "fd00::/8"},
		},
		{name: "tailnet path without public path", tailnet: "[fd7a:115c:a1e0::8]:2446", wantTailnet: "[fd7a:115c:a1e0::8]:2446"},
		{name: "public path requires trusted forwarders", public: "0.0.0.0:2445", wantError: true},
		{name: "public path rejects hostnames", public: "bot-api.example.test:2445", proxies: "10.0.0.0/8", wantError: true},
		{name: "public path rejects invalid proxy CIDRs", public: "0.0.0.0:2445", proxies: "0.0.0.0/0", wantError: true},
		{name: "tailnet path refuses a public address", tailnet: "192.0.2.8:2446", wantError: true},
		{name: "tailnet path requires a specific IP", tailnet: ":2446", wantError: true},
		{name: "proxy allowlist requires public path", proxies: "10.0.0.0/8", wantError: true},
		{name: "public path conflicts with legacy listener", public: "0.0.0.0:2445", tailnet: "100.64.0.8:2446", proxies: "10.0.0.0/8", wantError: true, legacy: "127.0.0.1:2445"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TG_LISTEN_ADDR", "127.0.0.1:2443")
			t.Setenv("TG_WEBSOCKET_LISTEN_ADDR", "")
			t.Setenv("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "")
			t.Setenv("TG_ADMIN_LISTEN_ADDR", "")
			t.Setenv("TG_ADMIN_TOKEN_HASH", "")
			t.Setenv("TG_ADMIN_ORIGIN", "")
			legacy := tt.legacy
			t.Setenv("TG_BOT_API_LISTEN_ADDR", legacy)
			t.Setenv("TG_BOT_API_PUBLIC_LISTEN_ADDR", tt.public)
			t.Setenv("TG_BOT_API_TAILNET_LISTEN_ADDR", tt.tailnet)
			t.Setenv("TG_BOT_API_PUBLIC_PROXY_CIDRS", tt.proxies)
			t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
			t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
			t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")

			cfg, err := config.Load(discardLog())
			if tt.wantError {
				if err == nil {
					t.Fatal("Load succeeded for an invalid Bot API path configuration")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BotAPIPublicListenAddr != tt.wantPublic {
				t.Errorf("BotAPIPublicListenAddr = %q, want %q", cfg.BotAPIPublicListenAddr, tt.wantPublic)
			}
			if cfg.BotAPITailnetListenAddr != tt.wantTailnet {
				t.Errorf("BotAPITailnetListenAddr = %q, want %q", cfg.BotAPITailnetListenAddr, tt.wantTailnet)
			}
			if len(cfg.BotAPIPublicProxyCIDRs) != len(tt.wantProxies) {
				t.Fatalf("BotAPIPublicProxyCIDRs = %v, want %v", cfg.BotAPIPublicProxyCIDRs, tt.wantProxies)
			}
			for i, want := range tt.wantProxies {
				if got := cfg.BotAPIPublicProxyCIDRs[i].String(); got != want {
					t.Errorf("BotAPIPublicProxyCIDRs[%d] = %q, want %q", i, got, want)
				}
			}
		})
	}
}
