package restoredrill_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"golang.org/x/sys/unix"

	restoredrill "github.com/teagramhq/teagram-server/test/e2e/restore_drill"
)

func TestRestoreDrillInvocationRequiresSyntheticOwnedLoopbackEvidence(t *testing.T) {
	tests := []struct {
		name       string
		invocation restoredrill.Invocation
		want       restoredrill.Category
	}{
		{
			name:       "all required attestations",
			invocation: invocationForEndpoint("ws://127.0.0.1:2444/apiws"),
			want:       restoredrill.CategoryAssertionsPassed,
		},
		{
			name: "missing scratch ownership evidence",
			invocation: func() restoredrill.Invocation {
				invocation := invocationForEndpoint("ws://127.0.0.1:2444/apiws")
				invocation.ScratchTargetOwned = false
				return invocation
			}(),
			want: restoredrill.CategoryInvocationInvalid,
		},
		{
			name: "missing network isolation evidence",
			invocation: func() restoredrill.Invocation {
				invocation := invocationForEndpoint("ws://127.0.0.1:2444/apiws")
				invocation.NetworkNamespaceIsolated = false
				return invocation
			}(),
			want: restoredrill.CategoryInvocationInvalid,
		},
		{
			name: "missing daemon identity evidence",
			invocation: func() restoredrill.Invocation {
				invocation := invocationForEndpoint("ws://127.0.0.1:2444/apiws")
				invocation.DaemonIdentityPinned = false
				return invocation
			}(),
			want: restoredrill.CategoryInvocationInvalid,
		},
		{
			name: "registration is not attested closed",
			invocation: func() restoredrill.Invocation {
				invocation := invocationForEndpoint("ws://127.0.0.1:2444/apiws")
				invocation.RegistrationClosed = false
				return invocation
			}(),
			want: restoredrill.CategoryInvocationInvalid,
		},
		{
			name: "restored input mode",
			invocation: func() restoredrill.Invocation {
				invocation := invocationForEndpoint("ws://127.0.0.1:2444/apiws")
				invocation.Mode = "restored-dump"
				return invocation
			}(),
			want: restoredrill.CategoryInvocationInvalid,
		},
		{
			name:       "non-loopback endpoint",
			invocation: invocationForEndpoint("wss://service.example.test/apiws"),
			want:       restoredrill.CategoryInvocationInvalid,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := restoredrill.ValidateInvocation(test.invocation); got != test.want {
				t.Fatalf("invocation category = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRestoreDrillLoadsProtectedInvocation(t *testing.T) {
	invocationPath := protectedTestInvocation(t)
	result := restoredrill.Run(context.Background(), invocationPath, filepath.Join(t.TempDir(), "missing-teagramd"))
	if result.Category != restoredrill.CategoryProvisioningFailure || result.Phase != restoredrill.PhaseProvisioning {
		t.Fatalf("valid protected invocation result = %q/%q, want provisioning_failure after validation", result.Phase, result.Category)
	}
	artifact, err := os.ReadFile(filepath.Join(filepath.Dir(invocationPath), "result.json")) // #nosec G304 -- test artifact is inside the private test invocation directory.
	if err != nil {
		t.Fatal("checker did not write a typed result artifact")
	}
	var artifactResult restoredrill.Result
	if err := json.Unmarshal(artifact, &artifactResult); err != nil {
		t.Fatal("checker result artifact was not valid JSON")
	}
	if artifactResult.Category != result.Category || artifactResult.Phase != result.Phase {
		t.Fatal("result artifact did not match the typed return value")
	}
}

func TestRestoreDrillRejectsUnprotectedInvocationFiles(t *testing.T) {
	t.Run("directory permissions", func(t *testing.T) {
		invocationPath := protectedTestInvocation(t)
		if err := unix.Chmod(filepath.Dir(invocationPath), 0o755); err != nil {
			t.Fatal("could not weaken test invocation directory")
		}
		result := restoredrill.Run(context.Background(), invocationPath, filepath.Join(t.TempDir(), "missing-teagramd"))
		if result.Category != restoredrill.CategoryInvocationInvalid {
			t.Fatalf("unprotected directory category = %q, want invocation_invalid", result.Category)
		}
	})
	t.Run("password permissions", func(t *testing.T) {
		invocationPath := protectedTestInvocation(t)
		if err := unix.Chmod(filepath.Join(filepath.Dir(invocationPath), "password"), 0o644); err != nil {
			t.Fatal("could not weaken test password permissions")
		}
		result := restoredrill.Run(context.Background(), invocationPath, filepath.Join(t.TempDir(), "missing-teagramd"))
		if result.Category != restoredrill.CategoryInvocationInvalid {
			t.Fatalf("unprotected password category = %q, want invocation_invalid", result.Category)
		}
	})
}

func TestRestoreDrillRejectsAmbiguousInvocationJSON(t *testing.T) {
	invocationPath := protectedTestInvocation(t)
	const ambiguousInvocation = `{"schema_version":1,"mode":"synthetic-only","endpoint":"ws://127.0.0.1:2444/apiws","scratch_target_owned":true,"network_namespace_isolated":true,"network_namespace_isolated":false,"daemon_identity_pinned":true,"registration_closed":true}`
	if err := os.WriteFile(invocationPath, []byte(ambiguousInvocation), 0o600); err != nil { // #nosec G703 -- test file is inside the private invocation directory.
		t.Fatal("could not prepare ambiguous invocation")
	}
	result := restoredrill.Run(context.Background(), invocationPath, filepath.Join(t.TempDir(), "missing-teagramd"))
	if result.Category != restoredrill.CategoryInvocationInvalid {
		t.Fatalf("duplicate invocation keys category = %q, want invocation_invalid", result.Category)
	}
}

func TestRestoreDrillConfigRequiresFreshValidDaemonContent(t *testing.T) {
	now := int(time.Now().Unix())
	valid := &tg.Config{
		ThisDC:               2,
		Date:                 now,
		Expires:              now + 3600,
		DCOptions:            []tg.DCOption{{ID: 2, IPAddress: "127.0.0.1", Port: 2443}},
		WebfileDCID:          2,
		ChatSizeMax:          200,
		MegagroupSizeMax:     200_000,
		ForwardedCountMax:    100,
		CaptionLengthMax:     1024,
		MessageLengthMax:     4096,
		OnlineUpdatePeriodMs: 120_000,
	}
	if !restoredrill.ValidConfig(valid) {
		t.Fatal("valid daemon configuration was rejected")
	}
	hostnameConfig := *valid
	hostnameConfig.DCOptions = append([]tg.DCOption(nil), valid.DCOptions...)
	hostnameConfig.DCOptions[0].IPAddress = "dc-1.example"
	if !restoredrill.ValidConfig(&hostnameConfig) {
		t.Fatal("valid hostname in daemon configuration was rejected")
	}

	tests := []struct {
		name   string
		mutate func(*tg.Config)
	}{
		{name: "missing data center", mutate: func(cfg *tg.Config) { cfg.ThisDC = 0 }},
		{name: "stale configuration", mutate: func(cfg *tg.Config) { cfg.Date -= 600 }},
		{name: "future configuration", mutate: func(cfg *tg.Config) { cfg.Date += 600 }},
		{name: "expired configuration", mutate: func(cfg *tg.Config) { cfg.Expires = cfg.Date }},
		{name: "missing matching data center option", mutate: func(cfg *tg.Config) { cfg.DCOptions = nil }},
		{name: "invalid data center port", mutate: func(cfg *tg.Config) { cfg.DCOptions[0].Port = 0 }},
		{name: "empty hostname label", mutate: func(cfg *tg.Config) { cfg.DCOptions[0].IPAddress = "dc..example" }},
		{name: "hostname label starts with hyphen", mutate: func(cfg *tg.Config) { cfg.DCOptions[0].IPAddress = "-dc.example" }},
		{name: "hostname label ends with hyphen", mutate: func(cfg *tg.Config) { cfg.DCOptions[0].IPAddress = "dc-.example" }},
		{name: "hostname label exceeds DNS limit", mutate: func(cfg *tg.Config) { cfg.DCOptions[0].IPAddress = strings.Repeat("a", 64) + ".example" }},
		{name: "hostname exceeds DNS limit", mutate: func(cfg *tg.Config) { cfg.DCOptions[0].IPAddress = strings.Repeat("a.", 127) + "a" }},
		{name: "missing size limit", mutate: func(cfg *tg.Config) { cfg.MessageLengthMax = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := *valid
			cfg.DCOptions = append([]tg.DCOption(nil), valid.DCOptions...)
			test.mutate(&cfg)
			if restoredrill.ValidConfig(&cfg) {
				t.Fatal("malformed daemon configuration was accepted")
			}
		})
	}
}

func TestRestoreDrillConfigDoesNotFollowRedirects(t *testing.T) {
	var sourceRequests, targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceRequests.Add(1)
		http.Redirect(w, r, target.URL+"/apiws", http.StatusFound)
	}))
	defer source.Close()

	endpoint := "ws://" + strings.TrimPrefix(source.URL, "http://") + "/apiws"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := restoredrill.CheckConfig(ctx, endpoint, validTestPublicKeyPEM(t))
	if sourceRequests.Load() == 0 {
		t.Fatal("loopback WebSocket endpoint did not receive a request")
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d request(s), want none", got)
	}
	if result.Category == restoredrill.CategoryAssertionsPassed {
		t.Fatal("redirect response was accepted as a valid daemon configuration")
	}
}

func TestRestoreDrillIdentityMustMatchCreateUserConfirmation(t *testing.T) {
	if !restoredrill.ValidIdentity(&tg.User{ID: 123, Username: "scratch_user"}, 123, "scratch_user") {
		t.Fatal("matching synthetic identity was rejected")
	}
	if restoredrill.ValidIdentity(&tg.User{ID: 124, Username: "scratch_user"}, 123, "scratch_user") {
		t.Fatal("unexpected user ID was accepted")
	}
	if restoredrill.ValidIdentity(&tg.User{ID: 123, Username: "other_user"}, 123, "scratch_user") {
		t.Fatal("unexpected username was accepted")
	}
	if restoredrill.ValidIdentity(nil, 123, "scratch_user") {
		t.Fatal("missing authenticated user was accepted")
	}
}

func TestRestoreDrillEndpointAcceptsOnlyLiteralLoopbackWebSocket(t *testing.T) {
	tests := []struct {
		endpoint string
		want     restoredrill.Category
	}{
		{endpoint: "ws://127.0.0.1:2444/apiws", want: restoredrill.CategoryAssertionsPassed},
		{endpoint: "ws://[::1]:2444/apiws", want: restoredrill.CategoryAssertionsPassed},
		{endpoint: "wss://127.0.0.1:2444/apiws", want: restoredrill.CategoryInvocationInvalid},
		{endpoint: "ws://localhost:2444/apiws", want: restoredrill.CategoryInvocationInvalid},
		{endpoint: "ws://198.51.100.2:2444/apiws", want: restoredrill.CategoryInvocationInvalid},
		{endpoint: "ws://127.0.0.1/apiws", want: restoredrill.CategoryInvocationInvalid},
		{endpoint: "ws://127.0.0.1:2444/not-apiws", want: restoredrill.CategoryInvocationInvalid},
		{endpoint: "ws://user@127.0.0.1:2444/apiws", want: restoredrill.CategoryInvocationInvalid},
	}
	for _, test := range tests {
		t.Run(test.endpoint, func(t *testing.T) {
			if got := restoredrill.ValidateInvocation(invocationForEndpoint(test.endpoint)); got != test.want {
				t.Fatalf("invocation category = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRestoreDrillResultDoesNotClaimOuterDrillOrCleanup(t *testing.T) {
	result := restoredrill.Result{
		SchemaVersion:   1,
		Phase:           restoredrill.PhaseApplication,
		Category:        restoredrill.CategoryAssertionsPassed,
		HelpGetConfig:   restoredrill.AssertionPass,
		SyntheticSignin: restoredrill.AssertionPass,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, forbidden := range [][]byte{[]byte(`"cleanup"`), []byte(`"drill_result"`), []byte(`"overall_pass"`)} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatal("checker result claims an outer orchestration outcome")
		}
	}
}

func TestRestoreDrillCheckTimeoutIsBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result := restoredrill.CheckConfig(ctx, "ws://127.0.0.1:2444/apiws", validTestPublicKeyPEM(t))
	if result.Category != restoredrill.CategoryTimeout {
		t.Fatalf("deadline category = %q, want %q", result.Category, restoredrill.CategoryTimeout)
	}
}

func invocationForEndpoint(endpoint string) restoredrill.Invocation {
	return restoredrill.Invocation{
		SchemaVersion:            1,
		Mode:                     "synthetic-only",
		Endpoint:                 endpoint,
		ScratchTargetOwned:       true,
		NetworkNamespaceIsolated: true,
		DaemonIdentityPinned:     true,
		RegistrationClosed:       true,
	}
}

func protectedTestInvocation(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := unix.Chmod(directory, 0o700); err != nil {
		t.Fatal("could not protect test invocation directory")
	}
	encoded, err := json.Marshal(invocationForEndpoint("ws://127.0.0.1:2444/apiws"))
	if err != nil {
		t.Fatal("could not encode test invocation")
	}
	writeProtectedTestFile(t, directory, "invocation.json", encoded, 0o600)
	writeProtectedTestFile(t, directory, "postgres.dsn", []byte("postgres://scratch:secret@127.0.0.1:5432/scratch?sslmode=disable\n"), 0o600)
	writeProtectedTestFile(t, directory, "authkey.hex", []byte(hex.EncodeToString(bytes.Repeat([]byte{0x42}, 32))), 0o600)
	writeProtectedTestFile(t, directory, "username", []byte("scratch_user\n"), 0o600)
	writeProtectedTestFile(t, directory, "password", []byte("Synthetic-Password\n"), 0o600)
	writeProtectedTestFile(t, directory, "server.pub.pem", validTestPublicKeyPEM(t), 0o400)
	return filepath.Join(directory, "invocation.json")
}

func writeProtectedTestFile(t *testing.T, directory, name string, content []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("could not write protected test file %q", name)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("could not protect test file %q", name)
	}
}

func validTestPublicKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("could not generate test key")
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal("could not encode test key")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
