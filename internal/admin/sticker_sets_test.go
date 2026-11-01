package admin_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/admin"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestStickerSetTakedownUnavailableFilesReturnsConflictAndKeepsSetLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to sticker-set database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close database connection: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	owner, err := st.CreateUser(ctx, "+15559319991")
	if err != nil {
		t.Fatalf("create sticker-set owner: %v", err)
	}
	data := []byte("sticker bytes")
	file, err := st.AllocateAndCompleteFile(ctx, owner.ID, int64(len(data)), "image/webp", "sticker.webp", 1<<20, nil, func(file store.File) error {
		_, putErr := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(data))
		return putErr
	})
	if err != nil {
		t.Fatalf("store sticker file: %v", err)
	}
	_, fingerprint, err := st.ReadStickerFileForSetWithFingerprint(ctx, file)
	if err != nil {
		t.Fatalf("read sticker file for validation: %v", err)
	}
	set, err := st.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Unavailable", ShortName: "unavailable_blob", Kind: "regular",
	}, []store.UserStickerSetItem{{
		FileID: file.ID, AccessHash: file.AccessHash, Size: file.Size,
		Emoji: "⭐", Format: "WEBP", Width: 512, Height: 512,
	}}, []store.StickerFileFingerprint{fingerprint})
	if err != nil {
		t.Fatalf("create sticker set: %v", err)
	}
	if err := blobs.Remove(ctx, blob.Key(file.ID)); err != nil {
		t.Fatalf("remove sticker blob: %v", err)
	}

	tokenHash := authTestTokenHash()
	sessionID := seedSession(t, ctx, st, tokenHash, time.Now().Add(time.Hour), time.Now())
	csrfToken, err := admin.SessionCSRFToken(tokenHash, sessionID)
	if err != nil {
		t.Fatalf("derive CSRF token: %v", err)
	}
	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))
	h := admin.AdminRouter(admin.LoginHandlerConfig{
		Store: st, TokenHash: tokenHash, Logger: logger,
	}, mtproto.NewSessionRegistry())
	form := url.Values{
		"csrf_token": {csrfToken},
		"short_name": {set.ShortName},
		"reason":     {"missing stored bytes"},
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/admin/sticker-sets/takedown", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionID}) //nolint:gosec // G124: test cookie
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("takedown status = %d, want 409; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), set.ShortName) {
		t.Fatalf("conflict message %q does not name set %q", rec.Body.String(), set.ShortName)
	}
	if !strings.Contains(logOutput.String(), set.ShortName) || !strings.Contains(logOutput.String(), "read file") {
		t.Fatalf("unavailable-file log %q does not include the set ref and wrapped read error", logOutput.String())
	}

	var isLive bool
	var events int
	if err := conn.QueryRow(ctx, `SELECT is_live FROM user_created_sticker_sets WHERE id = $1`, set.ID).Scan(&isLive); err != nil {
		t.Fatalf("read sticker-set state: %v", err)
	}
	if !isLive {
		t.Fatal("sticker set became unavailable after its blob read failed")
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM admin_sticker_set_events WHERE set_id = $1`, set.ID).Scan(&events); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if events != 0 {
		t.Fatalf("audit events after failed takedown = %d, want 0", events)
	}
}
