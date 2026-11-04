package admin_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func TestStickerSetTakedownUnavailableFilesSucceedsAndAuditsUnhashedCount(t *testing.T) {
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
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("takedown status = %d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logOutput.String(), set.ShortName) || !strings.Contains(logOutput.String(), "unhashed_files=1") {
		t.Fatalf("unavailable-file log %q does not include the set ref and unhashed file count", logOutput.String())
	}

	var isLive bool
	var events int
	if err := conn.QueryRow(ctx, `SELECT is_live FROM user_created_sticker_sets WHERE id = $1`, set.ID).Scan(&isLive); err != nil {
		t.Fatalf("read sticker-set state: %v", err)
	}
	if isLive {
		t.Fatal("sticker set stayed live after an item blob read failed")
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM admin_sticker_set_events WHERE set_id = $1`, set.ID).Scan(&events); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if events != 1 {
		t.Fatalf("audit events after takedown = %d, want 1", events)
	}
	var action, reason string
	if err := conn.QueryRow(ctx, `SELECT action, reason FROM admin_sticker_set_events WHERE set_id = $1`, set.ID).Scan(&action, &reason); err != nil {
		t.Fatalf("read takedown audit event: %v", err)
	}
	if action != "takedown" || reason != "missing stored bytes" {
		t.Fatalf("takedown event action/reason = %q/%q, want takedown/original operator reason", action, reason)
	}
	var contentSHA256 []byte
	if err := conn.QueryRow(ctx, `SELECT content_sha256 FROM user_created_sticker_set_items WHERE set_id = $1 AND file_id = $2`, set.ID, file.ID).Scan(&contentSHA256); err != nil {
		t.Fatalf("read unavailable item's content hash: %v", err)
	}
	wantHash := sha256.Sum256(data)
	if !bytes.Equal(contentSHA256, wantHash[:]) {
		t.Fatalf("unavailable item hash = %x, want retained creation hash %x", contentSHA256, wantHash)
	}
}

func TestStickerSetTakedownEnvelopeFailureSucceedsAndAudits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := testBlobs(t)
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x41}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create blob key ring: %v", err)
	}
	blobs, err := blob.NewEncrypted(backend, ring, blob.DefaultChunkSize)
	if err != nil {
		t.Fatalf("create encrypted blob store: %v", err)
	}
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

	owner, err := st.CreateUser(ctx, "+15559319992")
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
		CreatorID: owner.ID, Title: "Corrupt envelope", ShortName: "corrupt_envelope", Kind: "regular",
	}, []store.UserStickerSetItem{{
		FileID: file.ID, AccessHash: file.AccessHash, Size: file.Size,
		Emoji: "⭐", Format: "WEBP", Width: 512, Height: 512,
	}}, []store.StickerFileFingerprint{fingerprint})
	if err != nil {
		t.Fatalf("create sticker set: %v", err)
	}
	if _, err := backend.Put(ctx, blob.Key(file.ID), bytes.NewReader([]byte("garbage"))); err != nil {
		t.Fatalf("overwrite stored sticker with invalid envelope: %v", err)
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
		"reason":     {"corrupt stored bytes"},
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/admin/sticker-sets/takedown", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionID}) //nolint:gosec // G124: test cookie
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("takedown status = %d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	logText := logOutput.String()
	for _, want := range []string{set.ShortName, "file_id=" + strconv.FormatInt(file.ID, 10), "blob envelope invalid"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("takedown warning %q does not include %q", logText, want)
		}
	}

	var isLive bool
	if err := conn.QueryRow(ctx, `SELECT is_live FROM user_created_sticker_sets WHERE id = $1`, set.ID).Scan(&isLive); err != nil {
		t.Fatalf("read sticker-set state: %v", err)
	}
	if isLive {
		t.Fatal("sticker set stayed live after an envelope read failed")
	}
	var events int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM admin_sticker_set_events WHERE set_id = $1`, set.ID).Scan(&events); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if events != 1 {
		t.Fatalf("audit events after takedown = %d, want 1", events)
	}
	var contentSHA256 []byte
	if err := conn.QueryRow(ctx, `SELECT content_sha256 FROM user_created_sticker_set_items WHERE set_id = $1 AND file_id = $2`, set.ID, file.ID).Scan(&contentSHA256); err != nil {
		t.Fatalf("read corrupted item's content hash: %v", err)
	}
	wantHash := sha256.Sum256(data)
	if !bytes.Equal(contentSHA256, wantHash[:]) {
		t.Fatalf("corrupted item's hash = %x, want retained creation hash %x", contentSHA256, wantHash)
	}
}
