package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

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

	if output.Len() == 0 {
		t.Fatal("auth-key binding removal produced no diagnostic")
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("decode diagnostic: %v", err)
	}
	for field, want := range map[string]any{
		"operation":   "auth.logOut",
		"reason":      "client_logout",
		"outcome":     "cleared",
		"auth_key_id": float64(keyID),
		"user_id":     float64(owner.ID),
	} {
		if got := event[field]; got != want {
			t.Errorf("diagnostic %s = %v, want %v; record=%s", field, got, want, strings.TrimSpace(output.String()))
		}
	}
	if strings.Contains(output.String(), value) {
		t.Fatal("diagnostic contains auth-key bytes")
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
