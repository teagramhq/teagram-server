package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestLogOutAuthKeyWritesUnbindDiagnostic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const (
		keyID = int64(0x6f2906)
		value = "auth-key-bytes-must-never-be-logged"
	)
	var output bytes.Buffer
	s := openAuthKeyUnbindDiagnosticStore(t, &output, true)

	owner, err := s.CreateUser(ctx, "+15551262906")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.SaveAuthKey(ctx, keyID, []byte(value)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, keyID, owner.ID); err != nil {
		t.Fatalf("bind auth key: %v", err)
	}
	gotUserID, found, err := s.LogOutAuthKey(ctx, keyID)
	if err != nil || !found || gotUserID != owner.ID {
		t.Fatalf("logout = (%d, %v, %v), want (%d, true, nil)", gotUserID, found, err, owner.ID)
	}

	assertAuthKeyUnbindDiagnostic(t, &output, "auth.logOut", "client_logout", "cleared", keyID, owner.ID)
	if strings.Contains(output.String(), value) {
		t.Fatal("diagnostic contains auth-key bytes")
	}
}

func TestStagePendingUserWritesPasswordChallengeDiagnostic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var output bytes.Buffer
	s := openAuthKeyUnbindDiagnosticStore(t, &output, true)
	owner, err := s.CreateUser(ctx, "+15551262908")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const keyID = int64(0x6f2908)
	saveBoundAuthKey(t, ctx, s, keyID, owner.ID)
	if _, _, err := s.StagePendingUser(ctx, keyID, owner.ID, time.Hour); err != nil {
		t.Fatalf("stage pending user: %v", err)
	}

	assertAuthKeyUnbindDiagnostic(t, &output, "auth.signIn", "password_challenge", "pending", keyID, owner.ID)
}

func TestResetAuthorizationWritesClientResetDiagnostic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var output bytes.Buffer
	s := openAuthKeyUnbindDiagnosticStore(t, &output, true)
	owner, err := s.CreateUser(ctx, "+15551262909")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const keyID = int64(0x6f2909)
	saveBoundAuthKey(t, ctx, s, keyID, owner.ID)
	revoked, err := s.ResetAuthorization(ctx, owner.ID, keyID)
	if err != nil || !revoked {
		t.Fatalf("reset authorization = (%v, %v), want (true, nil)", revoked, err)
	}

	assertAuthKeyUnbindDiagnostic(t, &output, "account.resetAuthorization", "client_reset", "cleared", keyID, owner.ID)
}

func TestResetAuthorizationsWritesClientResetAllDiagnostic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var output bytes.Buffer
	s := openAuthKeyUnbindDiagnosticStore(t, &output, true)
	owner, err := s.CreateUser(ctx, "+15551262910")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const (
		callerKeyID = int64(0x6f2910)
		targetKeyID = int64(0x6f2911)
	)
	saveBoundAuthKey(t, ctx, s, callerKeyID, owner.ID)
	saveBoundAuthKey(t, ctx, s, targetKeyID, owner.ID)
	removed, err := s.ResetAuthorizations(ctx, owner.ID, callerKeyID)
	if err != nil || len(removed) != 1 || removed[0] != targetKeyID {
		t.Fatalf("reset authorizations = (%v, %v), want ([%d], nil)", removed, err, targetKeyID)
	}

	assertAuthKeyUnbindDiagnostic(t, &output, "auth.resetAuthorizations", "client_reset_all", "cleared", targetKeyID, owner.ID)
}

func TestSweepExpiredAuthorizationsWritesTTLUnbindDiagnostic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var output bytes.Buffer
	s := openAuthKeyUnbindDiagnosticStore(t, &output, true)
	owner, err := s.CreateUser(ctx, "+15551262912")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const keyID = int64(0x6f2912)
	saveBoundAuthKey(t, ctx, s, keyID, owner.ID)
	settings := requireAuthorizationTTLSettings(t, s)
	if err := settings.SetAuthorizationTTL(ctx, owner.ID, keyID, 7); err != nil {
		t.Fatalf("set authorization TTL: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, `
		UPDATE auth_keys
		SET last_seen_at = clock_timestamp() - interval '8 days'
		WHERE id = $1
	`, keyID); err != nil {
		t.Fatalf("expire auth key: %v", err)
	}

	revoked, err := s.SweepExpiredAuthorizations(ctx, 10)
	if err != nil || len(revoked) != 1 || revoked[0].ID != keyID || revoked[0].UserID != owner.ID {
		t.Fatalf("sweep expired authorizations = (%+v, %v), want only key %d for user %d", revoked, err, keyID, owner.ID)
	}
	assertAuthKeyUnbindDiagnostic(t, &output, "authorization_ttl_sweep", "authorization_ttl_expired", "cleared", keyID, owner.ID)
}

func assertAuthKeyUnbindDiagnostic(t *testing.T, output *bytes.Buffer, operation, reason, outcome string, keyID, userID int64) {
	t.Helper()
	if output.Len() == 0 {
		t.Fatal("auth-key binding removal produced no diagnostic")
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("decode diagnostic: %v", err)
	}
	for field, want := range map[string]any{
		"operation":   operation,
		"reason":      reason,
		"outcome":     outcome,
		"auth_key_id": float64(keyID),
		"user_id":     float64(userID),
	} {
		if got := event[field]; got != want {
			t.Errorf("diagnostic %s = %v, want %v; record=%s", field, got, want, strings.TrimSpace(output.String()))
		}
	}
}

func TestLogOutAuthKeyDiagnosticsAreDisabledByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var output bytes.Buffer
	s := openAuthKeyUnbindDiagnosticStore(t, &output, false)
	owner, err := s.CreateUser(ctx, "+15551262907")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const keyID = int64(0x6f2907)
	if err := s.SaveAuthKey(ctx, keyID, []byte("auth-key-bytes")); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, keyID, owner.ID); err != nil {
		t.Fatalf("bind auth key: %v", err)
	}
	if _, found, err := s.LogOutAuthKey(ctx, keyID); err != nil || !found {
		t.Fatalf("logout found=%v err=%v, want found=true and no error", found, err)
	}
	if output.Len() != 0 {
		t.Fatalf("default diagnostics wrote %q, want no output", output.String())
	}
}

func openAuthKeyUnbindDiagnosticStore(t *testing.T, output *bytes.Buffer, enabled bool) *store.Store {
	t.Helper()
	options := []store.Option{
		store.WithBlobStore(testBlobs(t)),
		store.WithLogger(slog.New(slog.NewJSONHandler(output, nil))),
	}
	if enabled {
		options = append(options, store.WithAuthKeyUnbindLogging(true))
	}
	s, err := store.Open(context.Background(), pgtest.DSN(t), pgtest.EncKey(), options...)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}
