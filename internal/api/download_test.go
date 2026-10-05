package api_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// downloadPayload is the body every fixture in this file uploads.
const downloadPayload = "hello world"

// downloadFixture has user a send downloadPayload as a document to user b,
// leaving a stored file plus a live message row on both sides.
func downloadFixture(t *testing.T, phoneA, phoneB string) (
	*store.Store, blob.Store, store.User, store.User, *tg.Document,
) {
	t.Helper()
	s := openStore(t)
	blobs := newBlobs(t)
	return downloadFixtureOn(t, s, blobs, phoneA, phoneB)
}

func downloadFixtureWithDSN(t *testing.T, phoneA, phoneB string) (
	*store.Store, string, blob.Store, store.User, store.User, *tg.Document,
) {
	t.Helper()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	_, _, a, b, doc := downloadFixtureOn(t, s, blobs, phoneA, phoneB)
	return s, dsn, blobs, a, b, doc
}

func downloadFixtureOn(t *testing.T, s *store.Store, blobs blob.Store, phoneA, phoneB string) (
	*store.Store, blob.Store, store.User, store.User, *tg.Document,
) {
	t.Helper()
	ctx := context.Background()
	a, err := s.CreateUser(ctx, phoneA)
	if err != nil {
		t.Fatalf("user a: %v", err)
	}
	b, err := s.CreateUser(ctx, phoneB)
	if err != nil {
		t.Fatalf("user b: %v", err)
	}
	saveParts(t, s, a.ID, 900, []byte(downloadPayload))
	enc, err := api.SendMediaForTest(s, a.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(a.ID, b.ID),
		Media:    uploadedDocument(900, 1, "hello.txt", "text/plain"),
		RandomID: 900,
	})
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	return s, blobs, a, b, documentOf(t, enc)
}

// getBytes runs one upload.getFile for userID and returns the bytes it served.
func getBytes(t *testing.T, s *store.Store, blobs blob.Store, userID int64, doc *tg.Document, offset int64, limit int) []byte {
	t.Helper()
	enc, err := api.GetFileForTest(s, userID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
		Offset:   offset,
		Limit:    limit,
	})
	if err != nil {
		t.Fatalf("get file offset=%d limit=%d: %v", offset, limit, err)
	}
	assertEncodes(t, enc)
	f, ok := enc.(*tg.UploadFile)
	if !ok {
		t.Fatalf("result type = %T, want *tg.UploadFile", enc)
	}
	if _, ok = f.Type.(*tg.StorageFileUnknown); !ok {
		t.Errorf("Type = %T, want *tg.StorageFileUnknown", f.Type)
	}
	return f.Bytes
}

type countingDownloadBlobStore struct {
	blob.Store

	reads atomic.Int64
}

func (b *countingDownloadBlobStore) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	b.reads.Add(1)
	return b.Store.ReadAt(ctx, key, offset, limit)
}

type blockingFirstDownloadBlobStore struct {
	blob.Store

	reads         atomic.Int64
	started       chan struct{}
	release       chan struct{}
	ignoreContext bool
	releaseOnce   sync.Once
}

func (b *blockingFirstDownloadBlobStore) releaseFirstRead() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingFirstDownloadBlobStore) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	if b.reads.Add(1) == 1 {
		close(b.started)
		if b.ignoreContext {
			<-b.release
		} else {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-b.release:
			}
		}
	}
	if b.ignoreContext {
		ctx = context.WithoutCancel(ctx)
	}
	return b.Store.ReadAt(ctx, key, offset, limit)
}

func assertFixedRateLimitLog(t *testing.T, h *captureHandler, message, sentinel string) {
	t.Helper()
	if len(h.records) != 1 {
		t.Fatalf("captured %d records, want one fixed rate-limit record", len(h.records))
	}
	record := h.records[0]
	if record.Message != message {
		t.Errorf("message = %q, want %q", record.Message, message)
	}
	if record.NumAttrs() != 0 {
		t.Errorf("record has %d attrs, want no dynamic attrs", record.NumAttrs())
	}
	if strings.Contains(record.Message, sentinel) {
		t.Errorf("record message contains sentinel %q", sentinel)
	}
}

func TestGetFileRateLimitReserveErrorDoesNotLogDynamicDetails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn, blobs, a, _, doc := downloadFixtureWithDSN(t, "+15551297081", "+15551297082")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	const sentinel = "get_file_reserve_error_sentinel"
	if _, err := conn.Exec(ctx, `
		ALTER TABLE rate_limits
		ADD CONSTRAINT get_file_reserve_error_sentinel CHECK (token_count < 0)
	`); err != nil {
		t.Fatalf("install reserve failure: %v", err)
	}

	logs := &captureHandler{}
	getFile := api.GetFileSeqForTestWithLimitsAndLogger(
		s, blobs, slog.New(logs),
		store.RateLimitConfig{Limit: 1, Window: time.Second},
		store.RateLimitConfig{},
	)
	_, err = getFile(a.ID, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
		Limit:    64,
	})
	if msg := rpcMessage(t, err); msg != "INTERNAL" {
		t.Fatalf("reserve failure = %s, want INTERNAL", msg)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("returned error contains sentinel %q: %v", sentinel, err)
	}
	assertFixedRateLimitLog(t, logs, "get file rate limit", sentinel)
}

func TestGetFileRateLimitRefundErrorDoesNotLogDynamicDetails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn, blobs, a, _, doc := downloadFixtureWithDSN(t, "+15551297091", "+15551297092")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	logs := &captureHandler{}
	getFile := api.GetFileSeqForTestWithLimitsAndLogger(
		s, blobs, slog.New(logs),
		store.RateLimitConfig{Limit: 2, Window: time.Second},
		store.RateLimitConfig{Limit: 1, Window: time.Second},
	)
	request := func() error {
		_, err := getFile(a.ID, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Limit:    64,
		})
		return err
	}
	if err := request(); err != nil {
		t.Fatalf("first getFile: %v", err)
	}

	if _, err := conn.Exec(ctx,
		`DELETE FROM rate_limits WHERE subject_id = $1 AND surface = 'upload_get_file'`,
		a.ID,
	); err != nil {
		t.Fatalf("clear account rate limit: %v", err)
	}

	const sentinel = "get_file_refund_error_sentinel"
	if _, err := conn.Exec(ctx, `
		ALTER TABLE rate_limits
		ADD CONSTRAINT get_file_refund_error_sentinel CHECK (token_count > 0) NOT VALID
	`); err != nil {
		t.Fatalf("install refund failure: %v", err)
	}
	err = request()
	if msg := rpcMessage(t, err); msg != "INTERNAL" {
		t.Fatalf("refund failure = %s, want INTERNAL", msg)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("returned error contains sentinel %q: %v", sentinel, err)
	}
	assertFixedRateLimitLog(t, logs, "get file rate limit refund", sentinel)
}

func TestGetFilePerAccountRateLimitAndReset(t *testing.T) {
	t.Parallel()
	s, dsn, blobs, a, _, doc := downloadFixtureWithDSN(t, "+15551297051", "+15551297052")
	getFile := api.GetFileSeqForTestWithLimits(
		s, blobs,
		store.RateLimitConfig{Limit: 2, Window: time.Second},
		store.RateLimitConfig{},
	)
	request := func() error {
		_, err := getFile(a.ID, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Limit:    64,
		})
		return err
	}

	for range 2 {
		if err := request(); err != nil {
			t.Fatalf("allowed getFile: %v", err)
		}
	}
	if msg := rpcMessage(t, request()); msg != "FLOOD_WAIT_1" {
		t.Fatalf("over-limit getFile = %s, want FLOOD_WAIT_1", msg)
	}
	if err := api.AgeRateLimitWindowForTest(dsn, a.ID, "upload_get_file", time.Second+time.Millisecond); err != nil {
		t.Fatalf("age getFile window: %v", err)
	}
	if err := request(); err != nil {
		t.Fatalf("getFile after reset: %v", err)
	}
}

func TestGetFileReplicaRateLimitIsSharedAcrossAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := &countingDownloadBlobStore{Store: newBlobs(t)}
	users := make([]store.User, 4)
	for i := range users {
		u, err := s.CreateUser(ctx, "+1555129706"+string(rune('0'+i)))
		if err != nil {
			t.Fatalf("user %d: %v", i, err)
		}
		users[i] = u
	}
	file, err := s.AllocateFile(ctx, users[0].ID, 7, "text/plain", "shared.txt", api.TestMaxUserStorageBytes)
	if err != nil {
		t.Fatalf("allocate file: %v", err)
	}
	if err := s.MarkFileStored(ctx, file.ID); err != nil {
		t.Fatalf("mark file stored: %v", err)
	}
	if _, err := blobs.Put(ctx, blob.Key(file.ID), strings.NewReader("payload")); err != nil {
		t.Fatalf("put blob: %v", err)
	}
	for i := 1; i < len(users); i++ {
		if _, _, _, _, err := s.SendMessage(ctx, users[0].ID, users[i].ID, "shared", int64(i), file.ID, 0); err != nil {
			t.Fatalf("entitle user %d: %v", i, err)
		}
	}
	getFile := api.GetFileSeqForTestWithLimits(
		s, blobs,
		store.RateLimitConfig{},
		store.RateLimitConfig{Limit: 3, Window: time.Second},
	)
	request := func(userID int64) error {
		_, err := getFile(userID, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: file.ID, AccessHash: file.AccessHash},
			Limit:    64,
		})
		return err
	}

	for i := range 3 {
		if err := request(users[i].ID); err != nil {
			t.Fatalf("account %d getFile: %v", i, err)
		}
	}
	if got := blobs.reads.Load(); got != 3 {
		t.Fatalf("blob reads before denial = %d, want 3", got)
	}
	if msg := rpcMessage(t, request(users[3].ID)); msg != "FLOOD_WAIT_1" {
		t.Fatalf("replica over-limit getFile = %s, want FLOOD_WAIT_1", msg)
	}
	if got := blobs.reads.Load(); got != 3 {
		t.Fatalf("blob reads after denial = %d, want 3", got)
	}

	stranger, err := s.CreateUser(ctx, "+15551297069")
	if err != nil {
		t.Fatalf("stranger: %v", err)
	}
	if msg := rpcMessage(t, request(stranger.ID)); msg != "LOCATION_INVALID" {
		t.Fatalf("unauthorized getFile while limited = %s, want LOCATION_INVALID", msg)
	}
	if got := blobs.reads.Load(); got != 3 {
		t.Fatalf("blob reads after unauthorized request = %d, want 3", got)
	}
}

func TestGetFileReplicaRateLimitIsSharedAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	firstStore, dsn, blobs, firstUser, secondUser, doc := downloadFixtureWithDSN(t, "+15551297101", "+15551297102")
	secondStore, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open second replica store: %v", err)
	}
	t.Cleanup(func() {
		if err := secondStore.Close(); err != nil {
			t.Errorf("close second replica store: %v", err)
		}
	})

	limit := store.RateLimitConfig{Limit: 2, Window: time.Second}
	firstReplica := api.GetFileSeqForTestWithLimits(firstStore, blobs, store.RateLimitConfig{}, limit)
	secondReplica := api.GetFileSeqForTestWithLimits(secondStore, blobs, store.RateLimitConfig{}, limit)
	request := func(userID int64, replica int) error {
		getFile := firstReplica
		if replica == 2 {
			getFile = secondReplica
		}
		_, err := getFile(userID, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Limit:    64,
		})
		return err
	}
	if err := request(firstUser.ID, 1); err != nil {
		t.Fatalf("first replica request: %v", err)
	}
	if err := request(secondUser.ID, 2); err != nil {
		t.Fatalf("second replica request: %v", err)
	}
	if msg := rpcMessage(t, request(firstUser.ID, 1)); msg != "FLOOD_WAIT_1" {
		t.Fatalf("combined replica limit = %s, want FLOOD_WAIT_1", msg)
	}
}

func TestGetFileInFlightSlotIsSharedAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	firstStore, dsn, localBlobs, user, _, doc := downloadFixtureWithDSN(t, "+15551297111", "+15551297112")
	secondStore, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open second replica store: %v", err)
	}
	t.Cleanup(func() {
		if err := secondStore.Close(); err != nil {
			t.Errorf("close second replica store: %v", err)
		}
	})

	blobs := &blockingFirstDownloadBlobStore{
		Store:   localBlobs,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	firstReplica := api.GetFileSeqForTestWithLimits(firstStore, blobs, store.RateLimitConfig{}, store.RateLimitConfig{})
	secondReplica := api.GetFileSeqForTestWithLimits(secondStore, blobs, store.RateLimitConfig{}, store.RateLimitConfig{})
	request := func(replica func(int64, *tg.UploadGetFileRequest) (bin.Encoder, error)) error {
		_, err := replica(user.ID, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Limit:    64,
		})
		return err
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- request(firstReplica) }()
	select {
	case <-blobs.started:
	case <-time.After(time.Second):
		close(blobs.release)
		t.Fatal("first replica did not reach the blob read")
	}

	if msg := rpcMessage(t, request(secondReplica)); msg != "FLOOD_WAIT_1" {
		close(blobs.release)
		t.Fatalf("second replica in-flight download = %s, want FLOOD_WAIT_1", msg)
	}
	close(blobs.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first replica download: %v", err)
	}
}

func TestGetFileInFlightLeaseRetriesTransientRenewalFailureAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	firstStore, dsn, localBlobs, user, _, doc := downloadFixtureWithDSN(t, "+15551297121", "+15551297122")
	secondStore, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open second replica store: %v", err)
	}
	t.Cleanup(func() {
		if err := secondStore.Close(); err != nil {
			t.Errorf("close second replica store: %v", err)
		}
	})

	blobs := &blockingFirstDownloadBlobStore{
		Store:         localBlobs,
		started:       make(chan struct{}),
		release:       make(chan struct{}),
		ignoreContext: true,
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	firstDone := make(chan struct{})
	firstResult := make(chan error, 1)
	go func() {
		_, err := api.GetFileForTestWithContext(requestCtx, firstStore, user.ID, blobs, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Limit:    64,
		})
		firstResult <- err
		close(firstDone)
	}()
	t.Cleanup(func() {
		blobs.releaseFirstRead()
		select {
		case <-firstDone:
		case <-time.After(5 * time.Second):
			t.Error("first replica download did not finish after test cleanup")
		}
	})
	select {
	case <-blobs.started:
	case <-time.After(5 * time.Second):
		blobs.releaseFirstRead()
		t.Fatal("first replica did not reach the blocked blob read")
	}

	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("connect to inspect initial lease expiry: %v", err)
	}
	t.Cleanup(func() {
		if err := lockConn.Close(context.Background()); err != nil {
			t.Errorf("close advisory lock connection: %v", err)
		}
	})
	var initialExpiry time.Time
	if err := lockConn.QueryRow(ctx, `
		SELECT expires_at
		FROM server_limit_leases
		WHERE subject_id = $1 AND surface = 'upload_get_file_in_flight'
	`, user.ID).Scan(&initialExpiry); err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("read initial lease expiry: %v", err)
	}
	var blockerPID int32
	if err := lockConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("read advisory lock backend pid: %v", err)
	}
	lockKey := fmt.Sprintf("%d:%s", user.ID, "upload_get_file_in_flight")
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1, hashtext($2))`, 0x74674c53, lockKey); err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("hold lease renewal lock: %v", err)
	}
	observerConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("connect to observe lease renewal: %v", err)
	}
	t.Cleanup(func() {
		if err := observerConn.Close(context.Background()); err != nil {
			t.Errorf("close lease renewal observer: %v", err)
		}
	})
	waitCtx, cancelWait := context.WithTimeout(ctx, 4*time.Second)
	defer cancelWait()
	renewalPID, err := waitForBlockedLeaseRenewal(waitCtx, observerConn, blockerPID)
	if err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("wait for lease renewal to block: %v", err)
	}
	var canceledRenewal bool
	if err := observerConn.QueryRow(ctx, `SELECT pg_cancel_backend($1)`, renewalPID).Scan(&canceledRenewal); err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("cancel transient lease renewal: %v", err)
	}
	if !canceledRenewal {
		blobs.releaseFirstRead()
		t.Fatal("Postgres did not cancel the blocked lease renewal")
	}
	if err := waitForLeaseRenewalIdle(waitCtx, observerConn, renewalPID); err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("wait for canceled lease renewal: %v", err)
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock($1, hashtext($2))`, 0x74674c53, lockKey); err != nil {
		blobs.releaseFirstRead()
		t.Fatalf("release lease renewal lock: %v", err)
	}
	if wait := time.Until(initialExpiry) + 100*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}

	select {
	case <-firstDone:
		blobs.releaseFirstRead()
		t.Fatalf("blocked first read completed before it was released: %v", <-firstResult)
	default:
	}
	_, secondErr := api.GetFileForTest(secondStore, user.ID, localBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
		Limit:    64,
	})
	if secondErr == nil {
		blobs.releaseFirstRead()
		t.Fatal("second replica was admitted after the original lease expired while the first read remained active")
	}
	if msg := rpcMessage(t, secondErr); msg != "FLOOD_WAIT_1" {
		blobs.releaseFirstRead()
		t.Fatalf("second replica after the original lease expiry = %s, want FLOOD_WAIT_1", msg)
	}

	blobs.releaseFirstRead()
	<-firstDone
	if err := <-firstResult; err == nil {
		t.Fatal("first replica download succeeded after transient renewal failure, want INTERNAL")
	} else if msg := rpcMessage(t, err); msg != "INTERNAL" {
		t.Fatalf("first replica download after transient renewal failure = %s, want INTERNAL", msg)
	}
}

func waitForBlockedLeaseRenewal(ctx context.Context, observer *pgx.Conn, blockerPID int32) (int32, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int32
		err := observer.QueryRow(ctx, `
			SELECT pid
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND wait_event = 'advisory'
			  AND $1 = ANY(pg_blocking_pids(pid))
			  AND query LIKE '%pg_advisory_xact_lock%'
			LIMIT 1
		`, blockerPID).Scan(&pid)
		if err == nil {
			return pid, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForLeaseRenewalIdle(ctx context.Context, observer *pgx.Conn, renewalPID int32) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var active bool
		err := observer.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE pid = $1
				  AND state = 'active'
				  AND query LIKE '%pg_advisory_xact_lock%'
			)
		`, renewalPID).Scan(&active)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestGetFileReplicaDenialRefundsAccountBudget(t *testing.T) {
	t.Parallel()
	s, dsn, blobs, account, _, doc := downloadFixtureWithDSN(t, "+15551297081", "+15551297082")
	getFile := api.GetFileSeqForTestWithLimits(
		s, blobs,
		store.RateLimitConfig{Limit: 2, Window: time.Minute},
		store.RateLimitConfig{Limit: 1, Window: time.Second},
	)
	request := func() error {
		_, err := getFile(account.ID, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Limit:    64,
		})
		return err
	}

	if err := request(); err != nil {
		t.Fatalf("first getFile: %v", err)
	}
	if msg := rpcMessage(t, request()); msg != "FLOOD_WAIT_1" {
		t.Fatalf("aggregate denial = %s, want FLOOD_WAIT_1", msg)
	}
	if err := api.AgeRateLimitWindowForTest(dsn, 0, "upload_get_file_replica", time.Second+time.Millisecond); err != nil {
		t.Fatalf("age aggregate getFile window: %v", err)
	}
	if err := request(); err != nil {
		t.Fatalf("getFile after aggregate window reset: %v", err)
	}
}

func TestGetFileRateLimitsCanBeDisabledIndependently(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		perAccount store.RateLimitConfig
		perReplica store.RateLimitConfig
		phoneA     string
		phoneB     string
	}{
		"per-account disabled": {
			perAccount: store.RateLimitConfig{},
			perReplica: store.RateLimitConfig{Limit: 2, Window: time.Second},
			phoneA:     "+15551297061",
			phoneB:     "+15551297062",
		},
		"replica disabled": {
			perAccount: store.RateLimitConfig{Limit: 2, Window: time.Second},
			perReplica: store.RateLimitConfig{},
			phoneA:     "+15551297071",
			phoneB:     "+15551297072",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, blobs, a, _, doc := downloadFixture(t, tc.phoneA, tc.phoneB)
			getFile := api.GetFileSeqForTestWithLimits(s, blobs, tc.perAccount, tc.perReplica)
			request := func() error {
				_, err := getFile(a.ID, &tg.UploadGetFileRequest{
					Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
					Limit:    64,
				})
				return err
			}
			for range 2 {
				if err := request(); err != nil {
					t.Fatalf("allowed getFile: %v", err)
				}
			}
			if msg := rpcMessage(t, request()); msg != "FLOOD_WAIT_1" {
				t.Fatalf("over-limit getFile = %s, want FLOOD_WAIT_1", msg)
			}
		})
	}
}

func TestGetFileRanges(t *testing.T) {
	t.Parallel()
	s, blobs, a, b, doc := downloadFixture(t, "+15551297001", "+15551297002")

	// Both the uploader and the recipient read the whole file: the gate is
	// ownership of a message row, not ownership of the upload.
	for name, uid := range map[string]int64{"uploader": a.ID, "recipient": b.ID} {
		if got := string(getBytes(t, s, blobs, uid, doc, 0, 64)); got != downloadPayload {
			t.Errorf("%s full read = %q, want %q", name, got, downloadPayload)
		}
	}

	if got := string(getBytes(t, s, blobs, a.ID, doc, 6, 5)); got != "world" {
		t.Errorf("ranged read = %q, want %q", got, "world")
	}
	// The last window of a file is short by definition, not an error.
	if got := string(getBytes(t, s, blobs, a.ID, doc, 8, api.MaxDownloadChunk)); got != "rld" {
		t.Errorf("short final window = %q, want %q", got, "rld")
	}
	// A client that has read to the end asks once more and gets an empty reply.
	if got := getBytes(t, s, blobs, a.ID, doc, int64(len(downloadPayload)), 1024); len(got) != 0 {
		t.Errorf("read at EOF = %q, want empty", got)
	}
}

func TestGetFileRejectsBadWindow(t *testing.T) {
	t.Parallel()
	s, blobs, a, _, doc := downloadFixture(t, "+15551297011", "+15551297012")

	windows := map[string]struct {
		offset int64
		limit  int
	}{
		"past end":      {int64(len(downloadPayload)) + 1, 1024},
		"zero limit":    {0, 0},
		"limit too big": {0, api.MaxDownloadChunk + 1},
		"negative":      {-1, 1024},
	}
	for name, w := range windows {
		_, err := api.GetFileForTest(s, a.ID, blobs, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Offset:   w.offset,
			Limit:    w.limit,
		})
		if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
			t.Errorf("%s: got %s, want LOCATION_INVALID", name, msg)
		}
	}
}

// TestGetFileGate pins that every way of not being entitled to a file returns
// the identical error, so the download path is not an enumeration oracle.
func TestGetFileGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, blobs, a, b, doc := downloadFixture(t, "+15551297021", "+15551297022")
	c, err := s.CreateUser(ctx, "+15551297023")
	if err != nil {
		t.Fatalf("user c: %v", err)
	}

	type rejection struct {
		userID int64
		loc    *tg.InputDocumentFileLocation
	}
	rejections := map[string]rejection{
		"stranger":    {c.ID, &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash}},
		"wrong hash":  {a.ID, &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash + 1}},
		"absent id":   {a.ID, &tg.InputDocumentFileLocation{ID: doc.ID + 1000, AccessHash: doc.AccessHash}},
		"thumb size":  {a.ID, &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, ThumbSize: "m"}},
		"never given": {c.ID, &tg.InputDocumentFileLocation{ID: doc.ID + 1000, AccessHash: doc.AccessHash + 1}},
	}

	// A file row that exists but whose bytes were never stored: allocated and
	// never assembled, so it fails the gate's stored predicate.
	unstored, err := s.AllocateFile(ctx, a.ID, 11, "text/plain", "never.txt", api.TestMaxUserStorageBytes)
	if err != nil {
		t.Fatalf("allocate file: %v", err)
	}
	rejections["not stored"] = rejection{
		a.ID, &tg.InputDocumentFileLocation{ID: unstored.ID, AccessHash: unstored.AccessHash},
	}

	for name, r := range rejections {
		_, err = api.GetFileForTest(s, r.userID, blobs, &tg.UploadGetFileRequest{
			Location: r.loc, Offset: 0, Limit: 1024,
		})
		if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
			t.Errorf("%s: got %s, want LOCATION_INVALID", name, msg)
		}
	}

	// A location type M5 does not serve is the same error again.
	_, err = api.GetFileForTest(s, a.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, ThumbSize: "m"},
		Offset:   0,
		Limit:    1024,
	})
	if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
		t.Errorf("photo location: got %s, want LOCATION_INVALID", msg)
	}

	// A garbage file reference still succeeds: it is ignored, not half-checked.
	enc, err := api.GetFileForTest(s, b.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{
			ID: doc.ID, AccessHash: doc.AccessHash, FileReference: []byte("garbage"),
		},
		Offset: 0,
		Limit:  1024,
	})
	if err != nil {
		t.Fatalf("garbage file reference: %v", err)
	}
	assertEncodes(t, enc)
}

// TestGetFileDeletedMessageRevokes pins that a one-sided delete (revoke=false,
// the client default) revokes retrieval only for the deleter: the sender keeps
// the message and the file, while the deleter gets LOCATION_INVALID. The
// revoke=true companion case below asserts both sides are revoked.
func TestGetFileDeletedMessageRevokes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, blobs, a, b, doc := downloadFixture(t, "+15551297031", "+15551297032")

	// b's own local id for the message, which is what b deletes by.
	hist, err := api.GetHistoryForTest(s, b.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(b.ID, a.ID),
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	msgs, ok := hist.(*tg.MessagesMessages)
	if !ok || len(msgs.Messages) != 1 {
		t.Fatalf("history = %#v, want one message", hist)
	}
	inbox, ok := msgs.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("history message type = %T", msgs.Messages[0])
	}

	// Both sides can read it before the delete.
	for _, uid := range []int64{a.ID, b.ID} {
		if got := string(getBytes(t, s, blobs, uid, doc, 0, 64)); got != downloadPayload {
			t.Fatalf("pre-delete read by %d = %q", uid, got)
		}
	}

	// b deletes her copy one-sidedly: only her retrieval is revoked.
	if _, err = s.DeleteMessages(ctx, b.ID, []int64{int64(inbox.ID)}, false); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if got := string(getBytes(t, s, blobs, a.ID, doc, 0, 64)); got != downloadPayload {
		t.Fatalf("sender read after one-sided delete = %q, want %q", got, downloadPayload)
	}
	_, err = api.GetFileForTest(s, b.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
		Offset:   0,
		Limit:    1024,
	})
	if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
		t.Errorf("deleter read after one-sided delete: got %s, want LOCATION_INVALID", msg)
	}

	// revoke=true deletes both rows and revokes both accounts.
	hist2, err := api.GetHistoryForTest(s, a.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(a.ID, b.ID),
	})
	if err != nil {
		t.Fatalf("get history a: %v", err)
	}
	outbox, ok := hist2.(*tg.MessagesMessages)
	if !ok || len(outbox.Messages) != 1 {
		t.Fatalf("history a = %#v, want one message", hist2)
	}
	out, ok := outbox.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("history a message type = %T", outbox.Messages[0])
	}
	if _, err = s.DeleteMessages(ctx, a.ID, []int64{int64(out.ID)}, true); err != nil {
		t.Fatalf("revoke delete: %v", err)
	}
	for _, uid := range []int64{a.ID, b.ID} {
		_, err = api.GetFileForTest(s, uid, blobs, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
			Offset:   0,
			Limit:    1024,
		})
		if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
			t.Errorf("post-revoke read by %d: got %s, want LOCATION_INVALID", uid, msg)
		}
	}
}

// TestGetFileReleasesSlotOnError drives the release through handleGetFile
// rather than through the slot methods, which is the only way to catch a
// dropped `defer h.endDownload`. That failure is permanent: a leaked slot locks
// the account out of downloading for the life of the process.
func TestGetFileReleasesSlotOnError(t *testing.T) {
	t.Parallel()
	s, blobs, a, _, doc := downloadFixture(t, "+15551297041", "+15551297042")
	// Both calls go through one handler, so they share the slot map.
	getFile := api.GetFileSeqForTest(s, blobs)

	// Fails after the claim: the window check that rejects an offset past the
	// end runs against the loaded file, so the slot has already been taken.
	_, err := getFile(a.ID, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
		Offset:   int64(len(downloadPayload)) + 1,
		Limit:    1024,
	})
	if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
		t.Fatalf("got %s, want LOCATION_INVALID", msg)
	}

	enc, err := getFile(a.ID, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash},
		Offset:   0,
		Limit:    64,
	})
	if err != nil {
		t.Fatalf("read after failed download: %v", err)
	}
	f, ok := enc.(*tg.UploadFile)
	if !ok {
		t.Fatalf("result type = %T, want *tg.UploadFile", enc)
	}
	if string(f.Bytes) != downloadPayload {
		t.Errorf("read after failed download = %q, want %q", f.Bytes, downloadPayload)
	}
}

func TestGetFileUnauthorized(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	_, err := api.GetFileForTest(s, 0, newBlobs(t), &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: 1, AccessHash: 1},
		Offset:   0,
		Limit:    1024,
	})
	if msg := rpcMessage(t, err); msg != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("got %s, want AUTH_KEY_UNREGISTERED", msg)
	}
}
