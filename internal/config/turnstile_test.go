package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestLoadRejectsAlwaysPassTurnstileTestSecret(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "site-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "1x0000000000000000000000000000000AA")

	_, err := config.Load(discardLog())
	if err == nil {
		t.Fatal("Load accepted Cloudflare's always-pass test secret")
	}
	if !strings.Contains(err.Error(), "TG_TURNSTILE_SECRET_KEY") {
		t.Fatalf("Load error = %q, want it to name the setting without exposing its value", err)
	}
	if strings.Contains(err.Error(), "1x0000000000000000000000000000000AA") {
		t.Fatalf("Load error exposed the secret: %q", err)
	}
}

func TestLoadTurnstileSecretFileEnablesSignUpChallenge(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "site-key_123")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	secretPath := filepath.Join(t.TempDir(), "turnstile-secret")
	if err := os.WriteFile(secretPath, []byte("private-key-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", secretPath)

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Turnstile.Enabled() || cfg.Turnstile.SiteKey != "site-key_123" || cfg.Turnstile.SecretKey != "private-key-123" {
		t.Fatalf("Turnstile config enabled=%v site-key-match=%v secret-loaded=%v, want all true", cfg.Turnstile.Enabled(), cfg.Turnstile.SiteKey == "site-key_123", cfg.Turnstile.SecretKey == "private-key-123")
	}
	encoded, err := json.Marshal(struct {
		Config any `json:"config"`
	}{Config: cfg})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(encoded), "private-key-123") {
		t.Fatalf("serialized config exposed the Siteverify secret: %s", encoded)
	}
}

func TestLoadWithoutTurnstileKeepsChallengeDisabled(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Turnstile.Enabled() || cfg.Turnstile.SiteKey != "" || cfg.Turnstile.SecretKey != "" {
		t.Fatalf("Turnstile config = %#v, want disabled", cfg.Turnstile)
	}
}

func TestLoadRejectsTurnstileSiteKeyWithoutSecret(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "site-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")

	_, err := config.Load(discardLog())
	if err == nil {
		t.Fatal("Load accepted a Turnstile site key without a verifier secret")
	}
	if !strings.Contains(err.Error(), "TG_TURNSTILE_SECRET_KEY") {
		t.Fatalf("Load error = %q, want it to name the missing secret setting", err)
	}
}
