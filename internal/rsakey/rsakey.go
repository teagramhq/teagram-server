// Package rsakey loads or bootstraps the server RSA key used in the MTProto
// auth-key exchange.
package rsakey

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gotd/td/crypto"
)

// Load returns the existing RSA private key at path. It never creates or
// changes the configured identity.
func Load(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the operator-configured server key file, not untrusted input.
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "RSA PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("invalid RSA private key PEM in %s", path)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("validate key: %w", err)
	}
	if key.N.BitLen() != crypto.RSAKeyBits {
		return nil, fmt.Errorf("RSA key in %s has %d bits, want %d", path, key.N.BitLen(), crypto.RSAKeyBits)
	}
	key.Precompute()
	return key, nil
}

// Bootstrap creates a new 2048-bit RSA identity at path. It publishes a fully
// written PKCS#1 PEM file atomically and refuses to replace any existing path.
func Bootstrap(path string) (key *rsa.PrivateKey, retErr error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("RSA key already exists at %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect RSA key destination: %w", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".bootstrap-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary RSA key: %w", err)
	}
	tmpPath := tmp.Name()
	cleanupTemp := true
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			if closeErr := tmp.Close(); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close temporary RSA key: %w", closeErr))
			}
		}
		if cleanupTemp {
			if removeErr := os.Remove(tmpPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("remove temporary RSA key: %w", removeErr))
			}
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure temporary RSA key: %w", err)
	}
	if _, err := tmp.Write(pemBytes); err != nil {
		return nil, fmt.Errorf("write temporary RSA key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("sync temporary RSA key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		tmpClosed = true
		return nil, fmt.Errorf("close temporary RSA key: %w", err)
	}
	tmpClosed = true
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("RSA key already exists at %s: %w", path, err)
		}
		return nil, fmt.Errorf("publish RSA key: %w", err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return nil, fmt.Errorf("remove temporary RSA key after publish: %w", err)
	}
	cleanupTemp = false

	dirFile, err := os.Open(dir) // #nosec G304 -- dir is derived from the operator-configured RSA key path and is only synced.
	if err != nil {
		return nil, fmt.Errorf("open RSA key directory for sync: %w", err)
	}
	syncErr := dirFile.Sync()
	closeErr := dirFile.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return nil, fmt.Errorf("sync RSA key directory: %w", err)
	}
	return key, nil
}

// Fingerprint returns the Telegram RSA fingerprint of the public key.
func Fingerprint(pub *rsa.PublicKey) int64 {
	return crypto.RSAFingerprint(pub)
}

// KeyID returns the SHA-256 of the DER SubjectPublicKeyInfo encoding of the
// public key, hex-encoded as 16 dash-separated groups of 4 characters
// (e.g. "a1b2-c3d4-e5f6-a7b8-..."). The grouping keeps the 64-character digest
// comparable by eye against the client UI; MAIN-314 renders the identical
// format.
func KeyID(pub *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal SubjectPublicKeyInfo: %w", err)
	}
	sum := sha256.Sum256(der)
	hexStr := hex.EncodeToString(sum[:])
	groups := make([]string, 0, 16)
	for i := 0; i < len(hexStr); i += 4 {
		groups = append(groups, hexStr[i:i+4])
	}
	return strings.Join(groups, "-"), nil
}
