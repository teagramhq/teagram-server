package rsakey_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/rsakey"
)

func TestBootstrapAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")

	first, err := rsakey.Bootstrap(path)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := rsakey.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if first.N.Cmp(second.N) != 0 {
		t.Error("key not persisted: modulus differs across loads")
	}
	if rsakey.Fingerprint(&first.PublicKey) == 0 {
		t.Error("fingerprint is zero")
	}
}

func TestLoadMissingKeyDoesNotCreateIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.pem")
	_, err := rsakey.Load(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load error = %v, want file-not-found", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("Load created missing key, stat error = %v", statErr)
	}
}

func TestLoadRejectsInvalidKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not an RSA key"), 0o600); err != nil {
		t.Fatalf("write invalid key: %v", err)
	}
	if _, err := rsakey.Load(path); err == nil || !strings.Contains(err.Error(), "invalid RSA private key PEM") {
		t.Fatalf("Load error = %v, want invalid-key error", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read invalid key after load: %v", err)
	}
	if string(data) != "not an RSA key" {
		t.Fatal("Load changed the invalid key file")
	}
}

func TestLoadRejectsUnreadableKeyPath(t *testing.T) {
	path := t.TempDir()
	if _, err := rsakey.Load(path); err == nil {
		t.Fatal("Load succeeded for a directory path, want a read failure")
	}
}
