package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunClientConfigIsIndependentAndAtomicOnWriterFailure(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "")
	t.Setenv("TG_AUTHKEY_ENC_KEY", "")
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SITE_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "")
	t.Setenv("TG_ADVERTISE_ADDR", "mtproto.example.com:443")
	t.Setenv("TG_DC_ID", "2")
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var bootstrapOutput bytes.Buffer
	if err := runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), &bootstrapOutput, &bootstrapOutput); err != nil {
		t.Fatalf("bootstrap-identity: %v", err)
	}

	var stdout bytes.Buffer
	if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &stdout, &stdout); err != nil {
		t.Fatalf("client-config: %v", err)
	}
	if stdout.Len() == 0 || !strings.Contains(stdout.String(), `"rsa_spki":"`) {
		t.Fatalf("client-config output = %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "PRIVATE KEY") {
		t.Fatalf("client-config output contains private-key material: %q", stdout.String())
	}
	first := bytes.Clone(stdout.Bytes())
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("client key was not persisted: %v", err)
	}

	stdout.Reset()
	if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &stdout, &stdout); err != nil {
		t.Fatalf("second client-config: %v", err)
	}
	if !bytes.Equal(stdout.Bytes(), first) {
		t.Fatalf("client-config output changed for the same key:\nfirst:  %s\nsecond: %s", first, stdout.Bytes())
	}

	stdout.Reset()
	if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &failWriter{}, &stdout); err == nil {
		t.Fatal("client-config succeeded after stdout write failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stderr after stdout failure = %q", stdout.String())
	}
}

func TestRunClientConfigDoesNotCreateMissingIdentity(t *testing.T) {
	t.Setenv("TG_ADVERTISE_ADDR", "mtproto.example.com:443")
	keyPath := filepath.Join(t.TempDir(), "missing-server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var stdout bytes.Buffer
	if err := runClientConfigCommand(&stdout); err == nil {
		t.Fatal("client-config succeeded with a missing RSA key")
	}
	if stdout.Len() != 0 {
		t.Fatalf("client-config wrote output after missing key: %q", stdout.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("client-config created missing RSA key, stat error = %v", err)
	}
}

func TestRunClientConfigAdvertisesTurnstileWidgetKey(t *testing.T) {
	t.Setenv("TG_ADVERTISE_ADDR", "mtproto.example.com:443")
	t.Setenv("TG_DC_ID", "2")
	t.Setenv("TG_TURNSTILE_SITE_KEY", "public-site-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "private-secret-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "")
	t.Setenv("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "")
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var bootstrapOutput bytes.Buffer
	if err := runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), &bootstrapOutput, &bootstrapOutput); err != nil {
		t.Fatalf("bootstrap-identity: %v", err)
	}
	var stdout bytes.Buffer
	if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &stdout, &stdout); err != nil {
		t.Fatalf("client-config: %v", err)
	}
	var document struct {
		Turnstile *struct {
			SiteKey string `json:"site_key"`
		} `json:"turnstile"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("decode client-config output: %v", err)
	}
	if document.Turnstile == nil || document.Turnstile.SiteKey != "public-site-key" {
		t.Fatalf("client-config Turnstile = %+v, want public-site-key", document.Turnstile)
	}
	if strings.Contains(stdout.String(), "secret") {
		t.Fatalf("client-config output contains private secret material: %q", stdout.String())
	}
}

func TestRunClientConfigDoesNotAdvertiseTurnstileOnTailnetWhenPublicListenerIsConfigured(t *testing.T) {
	t.Setenv("TG_ADVERTISE_ADDR", "mtproto.example.com:443")
	t.Setenv("TG_DC_ID", "2")
	t.Setenv("TG_TURNSTILE_SITE_KEY", "public-site-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "private-secret-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "")
	t.Setenv("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "127.0.0.1:2444")
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var bootstrapOutput bytes.Buffer
	if err := runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), &bootstrapOutput, &bootstrapOutput); err != nil {
		t.Fatalf("bootstrap-identity: %v", err)
	}
	var stdout bytes.Buffer
	if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &stdout, &stdout); err != nil {
		t.Fatalf("client-config: %v", err)
	}
	var document struct {
		Turnstile *struct {
			SiteKey string `json:"site_key"`
		} `json:"turnstile"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("decode client-config output: %v", err)
	}
	if document.Turnstile != nil {
		t.Fatalf("client-config Turnstile = %+v, want no signup key for tailnet discovery", document.Turnstile)
	}
}

func TestRunClientConfigAdvertisesDemoGuestSignUpOnlyWhenEnabled(t *testing.T) {
	t.Setenv("TG_ADVERTISE_ADDR", "mtproto.example.com:443")
	t.Setenv("TG_DC_ID", "2")
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var bootstrapOutput bytes.Buffer
	if err := runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), &bootstrapOutput, &bootstrapOutput); err != nil {
		t.Fatalf("bootstrap-identity: %v", err)
	}

	tests := []struct {
		name    string
		value   string
		set     bool
		enabled bool
	}{
		{name: "unset"},
		{name: "disabled", value: "false", set: true},
		{name: "enabled", value: "true", set: true, enabled: true},
	}
	var baseIdentity struct {
		Version int `json:"version"`
		MTProto struct {
			Endpoint string `json:"endpoint"`
			DCID     int    `json:"dc_id"`
			RSASPKI  string `json:"rsa_spki"`
		} `json:"mtproto"`
		WebSocket struct {
			Endpoint string `json:"endpoint"`
		} `json:"websocket"`
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("TG_ADVERTISE_DEMO_GUEST_SIGN_UP", tt.value)
			} else {
				t.Setenv("TG_ADVERTISE_DEMO_GUEST_SIGN_UP", "")
			}
			var stdout bytes.Buffer
			if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &stdout, &stdout); err != nil {
				t.Fatalf("client-config: %v", err)
			}

			var document struct {
				Version int `json:"version"`
				MTProto struct {
					Endpoint string `json:"endpoint"`
					DCID     int    `json:"dc_id"`
					RSASPKI  string `json:"rsa_spki"`
				} `json:"mtproto"`
				WebSocket struct {
					Endpoint string `json:"endpoint"`
				} `json:"websocket"`
				Demo *struct {
					GuestSignUp json.RawMessage `json:"guest_sign_up"`
				} `json:"demo"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
				t.Fatalf("decode client-config output %q: %v", stdout.String(), err)
			}
			if tt.enabled {
				if document.Demo == nil || !bytes.Equal(document.Demo.GuestSignUp, []byte("true")) {
					t.Fatalf("client-config demo capability = %s, want literal true", stdout.Bytes())
				}
			} else if document.Demo != nil {
				t.Fatalf("client-config includes demo while advertisement is disabled: %s", stdout.Bytes())
			}
			if i == 0 {
				baseIdentity.Version = document.Version
				baseIdentity.MTProto = document.MTProto
				baseIdentity.WebSocket = document.WebSocket
			} else if document.Version != baseIdentity.Version || document.MTProto != baseIdentity.MTProto || document.WebSocket != baseIdentity.WebSocket {
				t.Fatalf("client-config identity changed with advertisement mode: got %+v, want %+v", document, baseIdentity)
			}
		})
	}
}

func TestRunCommandClientConfigHelpDoesNotLoadIdentity(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "must-not-be-created.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var stdout, stderr bytes.Buffer
	if err := runCommand([]string{"client-config", "--help"}, slog.New(slog.DiscardHandler), &stdout, &stderr); err != nil {
		t.Fatalf("client-config help: %v", err)
	}
	if !strings.Contains(stdout.String(), "teagramd client-config") {
		t.Fatalf("help = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("help stderr = %q", stderr.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("help created RSA key, stat error = %v", err)
	}
}

func TestRunCommandBootstrapIdentityHelpDoesNotLoadIdentity(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "must-not-be-created.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var stdout, stderr bytes.Buffer
	if err := runCommand([]string{"bootstrap-identity", "--help"}, slog.New(slog.DiscardHandler), &stdout, &stderr); err != nil {
		t.Fatalf("bootstrap-identity help: %v", err)
	}
	if !strings.Contains(stdout.String(), "teagramd bootstrap-identity") {
		t.Fatalf("help = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("help stderr = %q", stderr.String())
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("bootstrap-identity help created RSA key, stat error = %v", err)
	}
}

type failWriter struct{}

func (*failWriter) Write([]byte) (int, error) { return 0, os.ErrPermission }
