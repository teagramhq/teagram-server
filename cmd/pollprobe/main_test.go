package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEndpointRequiresAnExplicitHostAndValidPort(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{name: "dns", endpoint: "probe.example:2443", wantHost: "probe.example", wantPort: 2443},
		{name: "ipv4", endpoint: "127.0.0.1:443", wantHost: "127.0.0.1", wantPort: 443},
		{name: "ipv6", endpoint: "[::1]:443", wantHost: "::1", wantPort: 443},
		{name: "missing port", endpoint: "probe.example", wantErr: true},
		{name: "url", endpoint: "https://probe.example:443", wantErr: true},
		{name: "zero port", endpoint: "probe.example:0", wantErr: true},
		{name: "overflow port", endpoint: "probe.example:65536", wantErr: true},
		{name: "malformed host", endpoint: "probe example:443", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseEndpoint(test.endpoint)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseEndpoint(%q) error = %v, wantErr %t", test.endpoint, err, test.wantErr)
			}
			if err == nil && (got.host != test.wantHost || got.port != test.wantPort) {
				t.Fatalf("parseEndpoint(%q) = %+v, want host %q port %d", test.endpoint, got, test.wantHost, test.wantPort)
			}
		})
	}
}

func TestParsePublicKeyRequiresThePinnedSPKIKeyID(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	digest := sha256.Sum256(der)
	keyID := hex.EncodeToString(digest[:])

	if _, err := parsePublicKey(block, keyID); err != nil {
		t.Fatalf("parse matching pinned public key: %v", err)
	}
	if _, err := parsePublicKey(block, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("parsePublicKey accepted a mismatched trusted key ID")
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := parsePublicKey(privatePEM, keyID); err == nil {
		t.Fatal("parsePublicKey accepted private key material")
	}
}

func TestLoadCredentialFilesRejectsGroupAccessibleDirectory(t *testing.T) {
	dir := writeProbeCredentialDirectory(t, 0o755)
	if _, err := loadCredentialFiles(dir); err == nil {
		t.Fatal("loadCredentialFiles accepted a group-accessible credential directory")
	}
}

func TestLoadCredentialFilesRejectsGroupReadablePassword(t *testing.T) {
	dir := writeProbeCredentialDirectory(t, 0o700)
	if err := os.Chmod(filepath.Join(dir, probeUsernames[0]+".password"), 0o640); err != nil { // #nosec G302 -- intentionally weaken permissions to test rejection.
		t.Fatal(err)
	}
	if _, err := loadCredentialFiles(dir); err == nil {
		t.Fatal("loadCredentialFiles accepted a group-readable password")
	}
}

func TestLoadCredentialFilesRejectsForeignOwnedDirectory(t *testing.T) {
	dir := writeProbeCredentialDirectory(t, 0o700)
	setForeignOwner(t, dir)
	if _, err := loadCredentialFiles(dir); err == nil {
		t.Fatal("loadCredentialFiles accepted a credential directory owned by another user")
	}
}

func TestLoadCredentialFilesRejectsForeignOwnedPassword(t *testing.T) {
	dir := writeProbeCredentialDirectory(t, 0o700)
	setForeignOwner(t, filepath.Join(dir, probeUsernames[0]+".password"))
	if _, err := loadCredentialFiles(dir); err == nil {
		t.Fatal("loadCredentialFiles accepted a password file owned by another user")
	}
}

func TestLoadCredentialFilesRejectsAdditionalPeerCredentials(t *testing.T) {
	dir := writeProbeCredentialDirectory(t, 0o700)
	if err := os.WriteFile(filepath.Join(dir, "other.password"), []byte("not-used\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentialFiles(dir); err == nil {
		t.Fatal("loadCredentialFiles accepted a fifth account credential")
	}
}

func TestLoadCredentialFilesRejectsSymlinkedPassword(t *testing.T) {
	dir := writeProbeCredentialDirectory(t, 0o700)
	passwordPath := filepath.Join(dir, probeUsernames[0]+".password")
	target := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(target, []byte("test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(passwordPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, passwordPath); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentialFiles(dir); err == nil {
		t.Fatal("loadCredentialFiles accepted a symlinked password")
	}
}

func TestRunRejectsArbitraryPeerFlagsWithoutEchoingTheirValue(t *testing.T) {
	const secretPeer = "arbitrary-peer-secret"
	var stdout, stderr bytes.Buffer
	err := run([]string{"--peer", secretPeer}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run accepted an arbitrary peer flag")
	}
	if strings.Contains(stdout.String()+stderr.String(), secretPeer) {
		t.Fatal("run echoed an arbitrary peer value")
	}
	if got := stderr.String(); got != "assertion=configuration_validated result=fail error_code=LOCAL_INPUT_INVALID\n" {
		t.Fatalf("sanitized configuration error = %q", got)
	}
}

func TestLoadCredentialFilesLoadsOnlyTheFixedFourAccounts(t *testing.T) {
	dir := writeProbeCredentialDirectory(t, 0o700)
	credentials, err := loadCredentialFiles(dir)
	if err != nil {
		t.Fatalf("loadCredentialFiles: %v", err)
	}
	defer clearCredentials(&credentials)
	for index, username := range probeUsernames {
		if credentials[index].username != username {
			t.Errorf("credential %d username = %q, want %q", index, credentials[index].username, username)
		}
		if string(credentials[index].password) != "test-password-"+username {
			t.Errorf("credential %d password was not loaded from its fixed file", index)
		}
	}
}

func writeProbeCredentialDirectory(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	for _, username := range probeUsernames {
		path := filepath.Join(dir, username+".password")
		if err := os.WriteFile(path, []byte("test-password-"+username+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func setForeignOwner(t *testing.T, path string) {
	t.Helper()
	owner := os.Geteuid()
	if err := os.Chown(path, owner+1, -1); err != nil {
		if os.IsPermission(err) {
			t.Skipf("changing ownership is required for this test: %v", err)
		}
		t.Fatalf("set foreign owner on %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := os.Chown(path, owner, -1); err != nil {
			t.Errorf("restore owner on %s: %v", path, err)
		}
	})
}
