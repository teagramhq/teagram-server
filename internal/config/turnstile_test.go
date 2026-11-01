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
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "")
	secretPath := filepath.Join(t.TempDir(), "turnstile-secret")
	if err := os.WriteFile(secretPath, []byte("private-key-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", secretPath)

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Turnstile.Enabled() || cfg.Turnstile.SiteKey != "site-key_123" || cfg.Turnstile.SecretKey != "private-key-123" || cfg.Turnstile.SignInEnabled {
		t.Fatalf("Turnstile config enabled=%v site-key-match=%v secret-loaded=%v signin-enabled=%v, want signup enabled and signin disabled", cfg.Turnstile.Enabled(), cfg.Turnstile.SiteKey == "site-key_123", cfg.Turnstile.SecretKey == "private-key-123", cfg.Turnstile.SignInEnabled)
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
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "")

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Turnstile.Enabled() || cfg.Turnstile.SiteKey != "" || cfg.Turnstile.SecretKey != "" || cfg.Turnstile.SignInEnabled {
		t.Fatalf("Turnstile config = %#v, want disabled", cfg.Turnstile)
	}
}

func TestLoadEnablesSigninOnlyWithConfiguredCredentials(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "site-key_123")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "private-key-123")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "true")

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Turnstile.Enabled() || !cfg.Turnstile.SignInEnabled {
		t.Fatalf("Turnstile config = %#v, want signup and signin enabled", cfg.Turnstile)
	}
}

func TestLoadDisablesSigninWithoutRemovingSignupCredentials(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "site-key_123")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "private-key-123")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "false")

	cfg, err := config.Load(discardLog())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Turnstile.Enabled() || cfg.Turnstile.SignInEnabled || cfg.Turnstile.SiteKey != "site-key_123" || cfg.Turnstile.SecretKey != "private-key-123" {
		t.Fatalf("Turnstile config = %#v, want signup credentials retained and signin disabled", cfg.Turnstile)
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

func TestLoadRejectsInvalidTurnstileSigninSetting(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "sometimes")

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_SIGNIN_ENABLED") {
		t.Fatalf("Load error = %v, want invalid TG_TURNSTILE_SIGNIN_ENABLED error", err)
	}
}

func TestLoadRejectsTurnstileSigninWithoutCredentials(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "true")

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_SIGNIN_ENABLED") {
		t.Fatalf("Load error = %v, want signin credentials error", err)
	}
}

func TestLoadAllowsCloudflareTestKeyPairsInExplicitTestMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		siteKey string
		secret  string
	}{
		{name: "always pass", siteKey: "1x00000000000000000000AA", secret: "1x0000000000000000000000000000000AA"},
		{name: "always block", siteKey: "2x00000000000000000000AB", secret: "2x0000000000000000000000000000000AA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTurnstileTestEnvironment(t)
			t.Setenv("TG_TURNSTILE_TEST_MODE", "true")
			t.Setenv("TG_TURNSTILE_SITE_KEY", tc.siteKey)
			t.Setenv("TG_TURNSTILE_SECRET_KEY", tc.secret)

			cfg, err := config.Load(discardLog())
			if err != nil {
				t.Fatalf("Load with Cloudflare test keys: %v", err)
			}
			if !cfg.Turnstile.Enabled() || cfg.Turnstile.SiteKey != tc.siteKey {
				t.Fatalf("Turnstile config = %#v, want enabled with site key %q", cfg.Turnstile, tc.siteKey)
			}
		})
	}
}

func TestLoadRejectsCloudflareTestKeysWithoutExplicitTestMode(t *testing.T) {
	setTurnstileTestEnvironment(t)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "2x00000000000000000000AB")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "2x0000000000000000000000000000000AA")

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_TEST_MODE") {
		t.Fatalf("Load error = %v, want it to require TG_TURNSTILE_TEST_MODE", err)
	}
}

func TestLoadRejectsRealSecretInTurnstileTestMode(t *testing.T) {
	setTurnstileTestEnvironment(t)
	t.Setenv("TG_TURNSTILE_TEST_MODE", "true")
	t.Setenv("TG_TURNSTILE_SITE_KEY", "real-site-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "real-secret-key")

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_TEST_MODE") {
		t.Fatalf("Load error = %v, want test mode to reject real Turnstile credentials", err)
	}
	if strings.Contains(err.Error(), "real-secret-key") {
		t.Fatalf("Load error exposed the configured secret: %q", err)
	}
}

func TestLoadRejectsTurnstileTestModeOnPublicTeagramHosts(t *testing.T) {
	for _, host := range []string{"demo.teagram.io:443", "api.teagram.io:443", "teagram.io:443"} {
		t.Run(host, func(t *testing.T) {
			setTurnstileTestEnvironment(t)
			t.Setenv("TG_TURNSTILE_TEST_MODE", "true")
			t.Setenv("TG_ADVERTISE_ADDR", host)

			_, err := config.Load(discardLog())
			if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_TEST_MODE") {
				t.Fatalf("Load error = %v, want test mode to reject public host %q", err, host)
			}
		})
	}
	t.Run("listen address fallback", func(t *testing.T) {
		setTurnstileTestEnvironment(t)
		t.Setenv("TG_TURNSTILE_TEST_MODE", "true")
		t.Setenv("TG_LISTEN_ADDR", "api.teagram.io:2443")

		_, err := config.Load(discardLog())
		if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_TEST_MODE") {
			t.Fatalf("Load error = %v, want test mode to reject public listen host", err)
		}
	})
}

func TestLoadValidatesTurnstileHostnameAllowlist(t *testing.T) {
	setTurnstileTestEnvironment(t)
	t.Setenv("TG_TURNSTILE_HOSTNAMES", "web.teagram.io, bad host")

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_HOSTNAMES") {
		t.Fatalf("Load error = %v, want an invalid hostname allowlist error", err)
	}
}

func TestLoadRejectsQABypassTokenOnProductionHost(t *testing.T) {
	setTurnstileTestEnvironment(t)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "real-site-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "real-secret-key")
	t.Setenv("TG_ADVERTISE_ADDR", "api.teagram.io:443")
	t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://teagram.io/")
	qaTokenPath := filepath.Join(t.TempDir(), "qa-token")
	t.Setenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", qaTokenPath)
	if err := os.WriteFile(qaTokenPath, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_QA_BYPASS_TOKEN_FILE") {
		t.Fatalf("Load error = %v, want production config to reject the QA bypass", err)
	}
}

func TestLoadAllowsQABypassTokenOnlyOnDemoHost(t *testing.T) {
	t.Run("demo host", func(t *testing.T) {
		setTurnstileTestEnvironment(t)
		t.Setenv("TG_TURNSTILE_SITE_KEY", "real-site-key")
		t.Setenv("TG_TURNSTILE_SECRET_KEY", "real-secret-key")
		t.Setenv("TG_ADVERTISE_ADDR", "demo.teagram.io:443")
		t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://demo.teagram.io/")
		qaTokenPath := filepath.Join(t.TempDir(), "qa-token")
		t.Setenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", qaTokenPath)
		if err := os.WriteFile(qaTokenPath, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
			t.Fatal(err)
		}

		cfg, err := config.Load(discardLog())
		if err != nil {
			t.Fatalf("Load demo QA bypass config: %v", err)
		}
		if cfg.Turnstile.QABypassToken != strings.Repeat("a", 64) {
			t.Fatal("Load did not return the configured demo QA bypass token")
		}
	})
	t.Run("demo host allowing the web origin", func(t *testing.T) {
		setTurnstileTestEnvironment(t)
		t.Setenv("TG_TURNSTILE_SITE_KEY", "real-site-key")
		t.Setenv("TG_TURNSTILE_SECRET_KEY", "real-secret-key")
		t.Setenv("TG_ADVERTISE_ADDR", "demo.teagram.io:443")
		t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://demo.teagram.io/")
		t.Setenv("TG_WEBSOCKET_ALLOWED_ORIGINS", "https://web.teagram.io,https://demo.teagram.io")
		qaTokenPath := filepath.Join(t.TempDir(), "qa-token")
		t.Setenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", qaTokenPath)
		if err := os.WriteFile(qaTokenPath, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := config.Load(discardLog()); err != nil {
			t.Fatalf("Load demo QA bypass config with web origin: %v", err)
		}
	})

	for _, host := range []string{"", "staging.example.test:443"} {
		t.Run("non-demo "+host, func(t *testing.T) {
			setTurnstileTestEnvironment(t)
			t.Setenv("TG_TURNSTILE_SITE_KEY", "real-site-key")
			t.Setenv("TG_TURNSTILE_SECRET_KEY", "real-secret-key")
			t.Setenv("TG_ADVERTISE_ADDR", host)
			qaTokenPath := filepath.Join(t.TempDir(), "qa-token")
			t.Setenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", qaTokenPath)
			if err := os.WriteFile(qaTokenPath, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := config.Load(discardLog())
			if err == nil || !strings.Contains(err.Error(), "demo.teagram.io") {
				t.Fatalf("Load error = %v, want QA bypass to require demo.teagram.io", err)
			}
		})
	}
	t.Run("demo origin alone is not a demo host", func(t *testing.T) {
		setTurnstileTestEnvironment(t)
		t.Setenv("TG_TURNSTILE_SITE_KEY", "real-site-key")
		t.Setenv("TG_TURNSTILE_SECRET_KEY", "real-secret-key")
		t.Setenv("TG_ADVERTISE_ADDR", "staging.example.test:443")
		t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://links.example.test/")
		t.Setenv("TG_WEBSOCKET_ALLOWED_ORIGINS", "https://demo.teagram.io")
		qaTokenPath := filepath.Join(t.TempDir(), "qa-token")
		t.Setenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", qaTokenPath)
		if err := os.WriteFile(qaTokenPath, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := config.Load(discardLog())
		if err == nil || !strings.Contains(err.Error(), "demo.teagram.io") {
			t.Fatalf("Load error = %v, want QA bypass to require demo public identity", err)
		}
	})
}

func TestLoadRejectsWeakQABypassToken(t *testing.T) {
	setTurnstileTestEnvironment(t)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "real-site-key")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "real-secret-key")
	t.Setenv("TG_ADVERTISE_ADDR", "demo.teagram.io:443")
	t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://demo.teagram.io/")
	qaTokenPath := filepath.Join(t.TempDir(), "qa-token")
	t.Setenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", qaTokenPath)
	if err := os.WriteFile(qaTokenPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(discardLog())
	if err == nil || !strings.Contains(err.Error(), "TG_TURNSTILE_QA_BYPASS_TOKEN_FILE") {
		t.Fatalf("Load error = %v, want a weak QA token error", err)
	}
}

func setTurnstileTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", validEncKey)
	t.Setenv("TG_TURNSTILE_SITE_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "")
	t.Setenv("TG_TURNSTILE_TEST_MODE", "")
	t.Setenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE", "")
	t.Setenv("TG_TURNSTILE_HOSTNAMES", "")
	t.Setenv("TG_ADVERTISE_ADDR", "")
	t.Setenv("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", "")
	t.Setenv("TG_WEBSOCKET_ALLOWED_ORIGINS", "")
	t.Setenv("TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS", "")
	t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://links.example.test/")
}
