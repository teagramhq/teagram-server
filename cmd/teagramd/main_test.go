package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobmode"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/photothumb"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPhotoThumbSupervisorFailureDisablesDerivativesWithoutAbortingStartup(t *testing.T) {
	var logs bytes.Buffer
	startupErr := errors.New("worker unavailable")
	var calls int
	supervisor := photoThumbSupervisorForStartup(slog.New(slog.NewTextHandler(&logs, nil)), func() (*photothumb.Supervisor, error) {
		calls++
		return nil, startupErr
	})
	if supervisor != nil {
		t.Fatalf("photo thumbnail supervisor = %v, want nil fallback", supervisor)
	}
	if calls != 1 {
		t.Fatalf("supervisor initializer calls = %d, want 1", calls)
	}
	if got := logs.String(); !strings.Contains(got, "photo thumbnail worker unavailable") || !strings.Contains(got, startupErr.Error()) {
		t.Fatalf("startup warning = %q, want worker-unavailable context and cause", got)
	}
}

func TestNewBlobStoreDefaultsToLocal(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "blobs")
	got, err := newBlobStore(context.Background(), config.Config{BlobDir: root}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new blob store: %v", err)
	}
	local, ok := got.(*blob.Local)
	if !ok {
		t.Fatalf("blob store type = %T, want *blob.Local", got)
	}
	if local.RootDir() != root {
		t.Fatalf("local root = %q, want %q", local.RootDir(), root)
	}
}

func TestNewBlobStoreChecksConfiguredS3AtStartup(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
			t.Errorf("startup request = %s %s, want S3 bucket listing", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	got, err := newBlobStore(context.Background(), config.Config{BlobS3: &blob.S3Config{
		Endpoint:          server.URL,
		Bucket:            "test-bucket",
		Prefix:            "tenant/",
		AccessKeyID:       "access",
		SecretAccessKey:   "secret",
		AllowInsecureHTTP: true,
		MaxAttempts:       1,
	}, MediaKeys: testMediaKeyRing(t)}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("new blob store: %v", err)
	}
	if _, ok := got.(*blob.Encrypted); !ok {
		t.Fatalf("blob store type = %T, want *blob.Encrypted", got)
	}
	if requests.Load() != 1 {
		t.Fatalf("startup requests = %d, want 1", requests.Load())
	}
}

func TestNewBlobStoreRequiresMediaKeysBeforeS3Access(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	_, err := newBlobStore(context.Background(), config.Config{BlobS3: &blob.S3Config{
		Endpoint:          server.URL,
		Bucket:            "test-bucket",
		Prefix:            "tenant/",
		AccessKeyID:       "access",
		SecretAccessKey:   "secret",
		AllowInsecureHTTP: true,
		MaxAttempts:       1,
	}}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "TG_MEDIA_KEY_DIR") || !strings.Contains(err.Error(), "TG_MEDIA_KEY_CURRENT") {
		t.Fatalf("new blob store error = %v, want missing media key settings", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("S3 requests before missing-key failure = %d, want 0", requests.Load())
	}
}

func TestNewBlobStoreKeepsAcceptedS3PlaintextRegardlessOfMediaKey(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys *blob.KeyRing
	}{
		{name: "without key"},
		{name: "with key", keys: testMediaKeyRing(t)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			const legacyMedia = "accepted plaintext media"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("S3 request = %s %s, want GET", r.Method, r.URL)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if r.URL.Query().Get("list-type") == "2" {
					w.Header().Set("Content-Type", "application/xml")
					if _, err := io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`); err != nil {
						t.Errorf("write S3 listing: %v", err)
					}
					return
				}
				if r.Header.Get("Range") != "bytes=0-23" {
					t.Errorf("S3 media range = %q, want bytes=0-23", r.Header.Get("Range"))
				}
				w.WriteHeader(http.StatusPartialContent)
				if _, err := io.WriteString(w, legacyMedia); err != nil {
					t.Errorf("write legacy media: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			got, err := newBlobStoreWithAuthority(context.Background(), config.Config{
				BlobS3: &blob.S3Config{
					Endpoint:          server.URL,
					Bucket:            "test-bucket",
					Prefix:            "tenant/",
					AccessKeyID:       "access",
					SecretAccessKey:   "secret",
					AllowInsecureHTTP: true,
					MaxAttempts:       1,
				},
				MediaKeys: tt.keys,
			}, slog.New(slog.NewTextHandler(&logs, nil)), blobmode.Authority{Present: true, Outcome: blobmode.OutcomeS3Accepted})
			if err != nil {
				t.Fatalf("new accepted S3 store: %v", err)
			}
			if _, ok := got.(*blob.S3); !ok {
				t.Fatalf("blob store type = %T, want plaintext *blob.S3", got)
			}
			if !strings.Contains(logs.String(), "S3 media encryption remains disabled for accepted plaintext cutover") {
				t.Fatalf("startup logs = %q, want accepted plaintext warning", logs.String())
			}
			data, err := got.ReadAt(context.Background(), "02/2", 0, int64(len(legacyMedia)))
			if err != nil {
				t.Fatalf("read accepted plaintext media: %v", err)
			}
			if string(data) != legacyMedia {
				t.Fatalf("accepted plaintext media = %q, want %q", data, legacyMedia)
			}
		})
	}
}

func testMediaKeyRing(t *testing.T) *blob.KeyRing {
	t.Helper()
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x51}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("new media key ring: %v", err)
	}
	return ring
}

func TestNewBlobStoreDoesNotFallBackWhenS3IsUnreachable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()
	root := filepath.Join(t.TempDir(), "blobs")
	_, err := newBlobStore(context.Background(), config.Config{BlobDir: root, BlobS3: &blob.S3Config{
		Endpoint:          endpoint,
		Bucket:            "test-bucket",
		Prefix:            "tenant/",
		AccessKeyID:       "access",
		SecretAccessKey:   "secret",
		AllowInsecureHTTP: true,
		MaxAttempts:       1,
		OperationTimeout:  100 * time.Millisecond,
	}, MediaKeys: testMediaKeyRing(t)}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "startup") {
		t.Fatalf("new blob store error = %v, want startup failure", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("local fallback created %q, stat error = %v", root, statErr)
	}
}

func TestRunValidatesPublicLinkPrefixBeforeLoadingServerConfig(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		wantAccept bool
	}{
		{name: "selected deployment", prefix: "https://teagram.tailaa4918.ts.net/", wantAccept: true},
		{name: "test origin", prefix: "https://links.example.test/", wantAccept: true},
		{name: "missing", prefix: ""},
		{name: "wrong scheme", prefix: "http://links.example.test/"},
		{name: "uppercase scheme", prefix: "HTTPS://links.example.test/"},
		{name: "uppercase host", prefix: "https://Links.example.test/"},
		{name: "port", prefix: "https://links.example.test:443/"},
		{name: "path", prefix: "https://links.example.test/path/"},
		{name: "query", prefix: "https://links.example.test/?q=1"},
		{name: "fragment", prefix: "https://links.example.test/#section"},
		{name: "userinfo", prefix: "https://user@links.example.test/"},
		{name: "ipv4 literal", prefix: "https://192.0.2.1/"},
		{name: "ipv6 literal", prefix: "https://[2001:db8::1]/"},
		{name: "trailing dot", prefix: "https://links.example.test./"},
		{name: "punycode", prefix: "https://xn--links.example/"},
		{name: "t.me", prefix: "https://t.me/"},
		{name: "t.me subdomain", prefix: "https://links.t.me/"},
		{name: "telegram.me", prefix: "https://telegram.me/"},
		{name: "telegram.me subdomain", prefix: "https://links.telegram.me/"},
		{name: "telegram.dog", prefix: "https://telegram.dog/"},
		{name: "telegram.dog subdomain", prefix: "https://links.telegram.dog/"},
		{name: "telegram.org", prefix: "https://telegram.org/"},
		{name: "telegram.org subdomain", prefix: "https://links.telegram.org/"},
		{name: "missing root slash", prefix: "https://links.example.test"},
		{name: "empty host", prefix: "https:///"},
		{name: "empty label", prefix: "https://links..example/"},
		{name: "underscore", prefix: "https://links_test.example/"},
		{name: "non-ascii", prefix: "https://línks.example/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TG_PUBLIC_LINK_PREFIX", tt.prefix)
			t.Setenv("TG_POSTGRES_DSN", "")
			t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("00", 32))
			t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")

			err := run(slog.New(slog.DiscardHandler))
			if tt.wantAccept {
				if err == nil || err.Error() != "TG_POSTGRES_DSN is required" {
					t.Fatalf("run error = %v, want config loading to continue to the missing Postgres DSN", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "TG_PUBLIC_LINK_PREFIX") {
				t.Fatalf("run error = %v, want TG_PUBLIC_LINK_PREFIX validation error", err)
			}
		})
	}
}

func TestRunFailsClosedWhenS3AccessKeyIsMissing(t *testing.T) {
	for _, name := range []string{
		"TG_BLOB_S3_ENDPOINT",
		"TG_BLOB_S3_BUCKET",
		"TG_BLOB_S3_PREFIX",
		"TG_BLOB_S3_REGION",
		"TG_BLOB_S3_ACCESS_KEY_ID",
		"TG_BLOB_S3_SECRET_ACCESS_KEY",
		"TG_BLOB_S3_SECRET_ACCESS_KEY_FILE",
		"TG_BLOB_S3_CA_PATH",
		"TG_BLOB_S3_ALLOW_INSECURE_HTTP",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("TG_PUBLIC_LINK_PREFIX", "https://links.example.test/")
	t.Setenv("TG_POSTGRES_DSN", "postgres://postgres:unused@localhost:5432/teagram?sslmode=disable")
	t.Setenv("TG_BLOB_S3_ENDPOINT", "http://rustfs:9000")
	t.Setenv("TG_BLOB_S3_BUCKET", "teagram")
	t.Setenv("TG_BLOB_S3_PREFIX", "teagramd/")
	t.Setenv("TG_BLOB_S3_SECRET_ACCESS_KEY_FILE", "/run/secrets/teagramd-blob-secret-key")

	err := run(slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "TG_BLOB_S3_ACCESS_KEY_ID") {
		t.Fatalf("run error = %v, want an error naming TG_BLOB_S3_ACCESS_KEY_ID", err)
	}
}

type commandRemoveProbe struct {
	blob.Store

	removed chan struct{}
	once    sync.Once
}

type cancelOnLogBuffer struct {
	bytes.Buffer

	cancel context.CancelFunc
}

func (b *cancelOnLogBuffer) Write(p []byte) (int, error) {
	n, err := b.Buffer.Write(p)
	b.cancel()
	return n, err
}

func (p *commandRemoveProbe) Remove(ctx context.Context, key string) error {
	p.once.Do(func() { close(p.removed) })
	return p.Store.Remove(ctx, key)
}

func TestMediaErasureReportsAccountForAllOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	u, err := s.CreateUser(ctx, "+15559301003")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	f, err := s.AllocateFile(ctx, u.ID, 1, "image/jpeg", "gallery.jpg", 1<<20)
	if err != nil {
		t.Fatalf("allocate gallery file: %v", err)
	}
	chatPhoto, err := s.AllocateFile(ctx, u.ID, 1, "image/jpeg", "group-photo.jpg", 1<<20)
	if err != nil {
		t.Fatalf("allocate group photo file: %v", err)
	}
	chat, err := s.CreateChat(ctx, u.ID, "Photo reference", nil)
	if err != nil {
		t.Fatalf("create photo-reference chat: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to insert gallery reference: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id)
		VALUES ($1, $2, $2)
	`, u.ID, f.ID); err != nil {
		if closeErr := conn.Close(ctx); closeErr != nil {
			t.Errorf("close gallery reference connection after insert failure: %v", closeErr)
		}
		t.Fatalf("insert gallery reference: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close gallery reference connection: %v", err)
	}
	conn, err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to insert chat photo reference: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		UPDATE chats
		SET current_photo_file_id = $2, current_photo_uploader_id = $3
		WHERE id = $1
	`, chat.ID, chatPhoto.ID, u.ID); err != nil {
		if closeErr := conn.Close(ctx); closeErr != nil {
			t.Errorf("close chat photo reference connection after update failure: %v", closeErr)
		}
		t.Fatalf("insert current chat photo reference: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close chat photo reference connection: %v", err)
	}

	assertOutcomePartition := func(t *testing.T, output, unreferencedKey, unassembledKey string) {
		t.Helper()
		fields := make(map[string]int)
		for field := range strings.FieldsSeq(output) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			count, err := strconv.Atoi(value)
			if err == nil {
				fields[key] = count
			}
		}
		scanned, ok := fields["scanned"]
		if !ok {
			t.Fatalf("report log = %q, missing scanned count", output)
		}
		if fields["skipped_gallery_ref"] != 1 || fields["skipped_chat_ref"] != 1 {
			t.Fatalf("report log = %q, want one gallery and one current chat photo reference", output)
		}
		outcomes := []string{
			unreferencedKey, unassembledKey,
			"skipped_message_ref", "skipped_channel_ref", "skipped_gallery_ref", "skipped_chat_ref", "skipped_too_new",
		}
		total := 0
		for _, key := range outcomes {
			count, present := fields[key]
			if !present {
				t.Errorf("report log = %q, missing outcome %s", output, key)
			}
			total += count
		}
		if total != scanned {
			t.Errorf("reported outcome total = %d, scanned = %d; log = %q", total, scanned, output)
		}
	}

	cutoff := config.Config{
		MediaErasureMinAge: time.Hour,
		BlobScanTempMinAge: time.Hour,
	}
	var sweepLog bytes.Buffer
	sweepMediaErasurePass(ctx, s, cutoff, slog.New(slog.NewTextHandler(&sweepLog, nil)))
	assertOutcomePartition(t, sweepLog.String(), "would_erase", "would_erase_unassembled")

	logCtx, cancel := context.WithCancel(ctx)
	candidateLog := &cancelOnLogBuffer{cancel: cancel}
	done := make(chan struct{})
	go func() {
		reportMediaErasureCandidates(logCtx, s, time.Hour, time.Hour, 100*time.Millisecond, slog.New(slog.NewTextHandler(candidateLog, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("candidate summary did not log before timeout")
	}
	assertOutcomePartition(t, candidateLog.String(), "unreferenced", "unassembled")
}

// The destructive off-switch is decided in cmd/teagramd, not in the Store.
// Calling the command-layer pass directly proves that an aged assembled temp
// remains untouched when the deployment is in its default reporting mode.
func TestMediaErasureDestructiveOffSwitchAtCommandBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	probe := &commandRemoveProbe{Store: l, removed: make(chan struct{})}
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(probe))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	u, err := s.CreateUser(ctx, "+15559301001")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	f, err := s.AllocateFile(ctx, u.ID, 1, "application/octet-stream", "upload.bin", 1<<20)
	if err != nil {
		t.Fatalf("allocate file: %v", err)
	}
	key := blob.Key(f.ID) + blob.TempSuffix
	if _, err := l.Put(ctx, key, bytes.NewReader([]byte("pending"))); err != nil {
		t.Fatalf("put temp: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(l.RootDir(), filepath.FromSlash(key)), old, old); err != nil {
		t.Fatalf("age temp: %v", err)
	}

	cfg := config.Config{
		MediaErasureDestructive: false,
		MediaErasureMinAge:      time.Hour,
		BlobScanTempMinAge:      time.Nanosecond,
	}
	log := slog.New(slog.DiscardHandler)
	sweepMediaErasurePass(ctx, s, cfg, log)

	select {
	case <-probe.removed:
		t.Fatal("reporting mode reached the blob backend's Remove")
	default:
	}
	if _, err := l.ReadAt(ctx, key, 0, 1); err != nil {
		t.Fatalf("reporting mode removed aged temp: %v", err)
	}
}

// The temporary-file age is only the gate for the disk-side temporary class.
// A not-stored row still uses the media age, so a temp cutoff shortened for
// disk cleanup cannot erase a row that has not crossed the media cutoff.
func TestMediaErasureUsesSeparateTempAndRowCutoffs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	l, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(l))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	u, err := s.CreateUser(ctx, "+15559301002")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	f, err := s.AllocateFile(ctx, u.ID, 1, "application/octet-stream", "upload.bin", 1<<20)
	if err != nil {
		t.Fatalf("allocate file: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to age row: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE files SET date = $2 WHERE id = $1`, f.ID, old); err != nil {
		_ = conn.Close(ctx) //nolint:errcheck // close after the failed setup query
		t.Fatalf("age file row: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close age connection: %v", err)
	}

	key := blob.Key(f.ID) + blob.TempSuffix
	if _, err := l.Put(ctx, key, bytes.NewReader([]byte("pending"))); err != nil {
		t.Fatalf("put temp: %v", err)
	}
	if err := os.Chtimes(filepath.Join(l.RootDir(), filepath.FromSlash(key)), old, old); err != nil {
		t.Fatalf("age temp: %v", err)
	}

	sweepMediaErasurePass(ctx, s, config.Config{
		MediaErasureDestructive: true,
		MediaErasureMinAge:      24 * time.Hour,
		BlobScanTempMinAge:      time.Hour,
	}, slog.New(slog.DiscardHandler))

	var rowExists bool
	conn, err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to inspect row: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM files WHERE id = $1)`, f.ID).Scan(&rowExists); err != nil {
		_ = conn.Close(ctx) //nolint:errcheck // close after the failed inspection query
		t.Fatalf("inspect file row: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close inspection connection: %v", err)
	}
	if !rowExists {
		t.Fatal("short temporary cutoff erased a not-stored row before the media cutoff")
	}
	if _, err := l.ReadAt(ctx, key, 0, 1); err != nil {
		t.Fatalf("short temporary cutoff removed the temporary: %v", err)
	}
}

func TestRunInviteCommandIssueAndList(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", pgtest.DSN(t))
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("00", 32))
	t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
	t.Setenv("TG_REGISTRATION", "open")
	blobDir := filepath.Join(t.TempDir(), "blobs")
	rsaKeyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_BLOB_DIR", blobDir)
	t.Setenv("TG_RSA_KEY_PATH", rsaKeyPath)

	log := slog.New(slog.DiscardHandler)
	var stdout, stderr bytes.Buffer
	if err := runInviteCommand([]string{"issue", "Alice", "--lifetime", "48h"}, log, &stdout, &stderr); err != nil {
		t.Fatalf("issue command: %v", err)
	}
	secret := strings.TrimSpace(stdout.String())
	if len(secret) != 64 {
		t.Fatalf("secret = %q, want 64 characters", secret)
	}
	if strings.Trim(secret, "0123456789abcdef") != "" {
		t.Fatalf("secret = %q, want lowercase hex", secret)
	}
	if !strings.Contains(stderr.String(), "Invite issued: id=") || !strings.Contains(stderr.String(), "handle=alice") {
		t.Fatalf("issue metadata = %q, want id and canonical handle", stderr.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatal("issue metadata contains the secret")
	}
	var id int64
	if _, err := fmt.Sscanf(stderr.String(), "Invite issued: id=%d", &id); err != nil {
		t.Fatalf("issue metadata = %q, parse id: %v", stderr.String(), err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runInviteCommand([]string{"list"}, log, &stdout, &stderr); err != nil {
		t.Fatalf("list command: %v", err)
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatal("list output contains the secret")
	}
	for _, want := range []string{"ID", "HANDLE", "STATE", "ISSUED", "EXPIRES", "alice", "issued"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("list output = %q, missing %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("list stderr = %q, want empty", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	err := runInviteCommand([]string{"issue", "alice"}, log, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "alice") {
		t.Fatalf("duplicate issue error = %v, want an error naming alice", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("duplicate issue stdout = %q, want empty", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runInviteCommand([]string{"revoke", strconv.FormatInt(id, 10)}, log, &stdout, &stderr); err != nil {
		t.Fatalf("revoke command: %v", err)
	}
	if stdout.Len() != 0 || stderr.String() != fmt.Sprintf("Revoked: id=%d\n", id) {
		t.Fatalf("revoke output = stdout %q stderr %q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runInviteCommand([]string{"list"}, log, &stdout, &stderr); err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if !strings.Contains(stdout.String(), "revoked") {
		t.Fatalf("list after revoke = %q, missing revoked state", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("list after revoke stderr = %q, want empty", stderr.String())
	}

	if _, err := os.Stat(blobDir); !os.IsNotExist(err) {
		t.Fatalf("invite command initialized blob dir, stat error = %v", err)
	}
	if _, err := os.Stat(rsaKeyPath); !os.IsNotExist(err) {
		t.Fatalf("invite command initialized RSA key, stat error = %v", err)
	}
}

func TestParseInviteIssueArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		handle   string
		lifetime time.Duration
		wantErr  bool
	}{
		{name: "default", args: []string{"Alice"}, handle: "Alice"},
		{name: "explicit", args: []string{"alice", "--lifetime", "48h"}, handle: "alice", lifetime: 48 * time.Hour},
		{name: "missing handle", wantErr: true},
		{name: "unknown option", args: []string{"alice", "--other", "48h"}, wantErr: true},
		{name: "invalid duration", args: []string{"alice", "--lifetime", "later"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handle, lifetime, err := parseInviteIssueArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatal("parse succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if handle != tc.handle || lifetime != tc.lifetime {
				t.Fatalf("parse = handle %q lifetime %s, want %q %s", handle, lifetime, tc.handle, tc.lifetime)
			}
		})
	}
}
