package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/rsakey"
)

func setIdentityRunConfig(t *testing.T, keyPath string) {
	t.Helper()
	t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://links.example.test/")
	t.Setenv("TG_POSTGRES_DSN", "invalid dsn")
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("00", 32))
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	t.Setenv("TG_RSA_KEY_FINGERPRINT", "")
	t.Setenv("TG_BLOB_DIR", filepath.Join(t.TempDir(), "blobs"))
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_REPLICA_ID", "")
	t.Setenv("TG_ADMIN_LISTEN_ADDR", "")
	t.Setenv("TG_ADMIN_ORIGIN", "")
	t.Setenv("TG_ADMIN_TOKEN_HASH", "")
	t.Setenv("TG_WEBSOCKET_LISTEN_ADDR", "")
	t.Setenv("TG_CLIENT_ADDR_TRUST", "socket")
	t.Setenv("TG_CLIENT_ADDR_PROXY_CIDRS", "")
}

func TestRunFailsClosedWhenRSAIdentityIsMissing(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	setIdentityRunConfig(t, keyPath)

	err := run(slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), keyPath) {
		t.Fatalf("run error = %v, want an RSA key load error naming the configured path", err)
	}
	if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
		t.Fatalf("serving created the missing RSA key, stat error = %v", statErr)
	}
}

func TestRunRejectsConfiguredRSAFingerprintMismatchBeforeStoreOpen(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	key, err := rsakey.Bootstrap(keyPath)
	if err != nil {
		t.Fatalf("create existing RSA key: %v", err)
	}
	setIdentityRunConfig(t, keyPath)
	want := rsakey.Fingerprint(&key.PublicKey) + 1
	t.Setenv("TG_RSA_KEY_FINGERPRINT", strconv.FormatInt(want, 10))

	err = run(slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "TG_RSA_KEY_FINGERPRINT") {
		t.Fatalf("run error = %v, want configured RSA fingerprint mismatch before opening the invalid database DSN", err)
	}
}

func TestBootstrapIdentityCommandPublishesOnePrivateKeyConcurrently(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	type result struct {
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			results <- result{err: runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), io.Discard, io.Discard)}
		}()
	}

	successes := 0
	refusals := 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			successes++
		case strings.Contains(strings.ToLower(got.err.Error()), "exist"):
			refusals++
		default:
			t.Fatalf("bootstrap error = %v, want a clear existing-destination refusal", got.err)
		}
	}
	if successes != 1 || refusals != 1 {
		t.Fatalf("bootstrap successes=%d refusals=%d, want one each", successes, refusals)
	}
	key, err := rsakey.Load(keyPath)
	if err != nil {
		t.Fatalf("load published key: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat published key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("published key permissions = %04o, want 0600", info.Mode().Perm())
	}
	if rsakey.Fingerprint(&key.PublicKey) == 0 {
		t.Fatal("published key has zero fingerprint")
	}
}

func TestBootstrapIdentityRefusesToOverwriteExistingKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	if err := runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), io.Discard, io.Discard); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read first key: %v", err)
	}
	if err := runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), io.Discard, io.Discard); err == nil {
		t.Fatal("second bootstrap succeeded, want existing-destination refusal")
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key after refused bootstrap: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("refused bootstrap changed the existing key")
	}
}
