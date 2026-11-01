package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type stickerSetReadGate struct {
	blob.Store

	mu      sync.Mutex
	key     string
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (g *stickerSetReadGate) arm(key string) (<-chan struct{}, chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.key = key
	g.armed = true
	g.entered = make(chan struct{})
	g.release = make(chan struct{})
	return g.entered, g.release
}

func (g *stickerSetReadGate) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	g.mu.Lock()
	block := g.armed && key == g.key
	entered, release := g.entered, g.release
	if block {
		g.armed = false
	}
	g.mu.Unlock()
	if block {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.Store.ReadAt(ctx, key, offset, limit)
}

func openStickerSetReadGateStore(t *testing.T, blobs *stickerSetReadGate, dsns ...string) *store.Store {
	t.Helper()
	var dsn string
	if len(dsns) > 0 {
		dsn = dsns[0]
	} else {
		dsn = pgtest.DSN(t)
	}
	s, err := store.Open(context.Background(), dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
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

func stickerSetFileWithBytes(t *testing.T, s *store.Store, owner store.User, data []byte) store.File {
	t.Helper()
	file := storedFile(t, s, owner.ID)
	if int64(len(data)) != file.Size {
		t.Fatalf("sticker fixture has %d bytes, file row has size %d", len(data), file.Size)
	}
	if _, err := store.BlobsOf(s).Put(context.Background(), blob.Key(file.ID), bytes.NewReader(data)); err != nil {
		t.Fatalf("store sticker fixture bytes: %v", err)
	}
	return file
}

func createOneFileStickerSet(ctx context.Context, s *store.Store, owner store.User, file store.File, title, shortName string, data []byte) (store.UserStickerSet, error) {
	return s.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: title, ShortName: shortName, Kind: "regular",
	}, []store.UserStickerSetItem{{
		FileID: file.ID, AccessHash: file.AccessHash, Size: file.Size,
		Emoji: "⭐", Format: "WEBP", Width: 512, Height: 512,
	}})
}

func TestCreateUserStickerSetStoresFingerprintFromBlob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStickerSetReadGateStore(t, &stickerSetReadGate{Store: testBlobs(t)})
	owner := mustUser(t, s, "+15559310003")
	data := []byte("hello world")
	file := stickerSetFileWithBytes(t, s, owner, data)
	set, err := s.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: "Digest", ShortName: "digest_source", Kind: "regular",
	}, []store.UserStickerSetItem{{
		FileID: file.ID, AccessHash: file.AccessHash, Size: file.Size,
		Emoji: "⭐", Format: "WEBP", Width: 512, Height: 512,
	}})
	if err != nil {
		t.Fatalf("create sticker set: %v", err)
	}
	var got []byte
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT content_sha256 FROM user_created_sticker_set_items WHERE set_id = $1 AND file_id = $2
	`, set.ID, file.ID).Scan(&got); err != nil {
		t.Fatalf("read stored content fingerprint: %v", err)
	}
	want := sha256.Sum256(data)
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("stored fingerprint = %x, want hash of blob %x", got, want)
	}
}

func stickerSetAdminRefByID(t *testing.T, id int64) store.StickerSetAdminRef {
	t.Helper()
	ref, err := store.StickerSetAdminRefByID(id)
	if err != nil {
		t.Fatalf("create sticker-set admin reference: %v", err)
	}
	return ref
}

func TestStickerSetTakedownDoesNotHoldInterlockDuringBlobRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	blobs := &stickerSetReadGate{Store: testBlobs(t)}
	s := openStickerSetReadGateStore(t, blobs)
	owner := mustUser(t, s, "+15559310001")
	creatingOwner := mustUser(t, s, "+15559310002")
	setData := []byte("hello world")
	createData := []byte("other value")
	setFile := stickerSetFileWithBytes(t, s, owner, setData)
	set, err := createOneFileStickerSet(ctx, s, owner, setFile, "Slow blob", "slow_blob", setData)
	if err != nil {
		t.Fatalf("create sticker set: %v", err)
	}
	createFile := stickerSetFileWithBytes(t, s, creatingOwner, createData)
	entered, release := blobs.arm(blob.Key(setFile.ID))
	var releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRead()

	ref := stickerSetAdminRefByID(t, set.ID)
	takedownDone := make(chan error, 1)
	go func() {
		takedownDone <- s.TakedownUserStickerSet(ctx, ref, "test", make([]byte, sha256.Size))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("takedown did not reach the gated blob read")
	}

	createDone := make(chan error, 1)
	go func() {
		_, createErr := createOneFileStickerSet(ctx, s, creatingOwner, createFile, "Concurrent", "concurrent", createData)
		createDone <- createErr
	}()
	select {
	case createErr := <-createDone:
		if createErr != nil {
			releaseRead()
			takedownErr := <-takedownDone
			t.Fatalf("create while takedown read is paused: %v (takedown: %v)", createErr, takedownErr)
		}
	case <-time.After(3 * time.Second):
		releaseRead()
		takedownErr := <-takedownDone
		createErr := <-createDone
		t.Fatalf("sticker creation waited for takedown blob read (takedown: %v, create: %v)", takedownErr, createErr)
	}
	releaseRead()
	if err := <-takedownDone; err != nil {
		t.Fatalf("takedown after blob read: %v", err)
	}
}

func TestStickerSetTakedownRevalidatesAfterBlobRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	blobs := &stickerSetReadGate{Store: testBlobs(t)}
	dsn := pgtest.DSN(t)
	s := openStickerSetReadGateStore(t, blobs, dsn)
	owner := mustUser(t, s, "+15559310011")
	data := []byte("hello world")
	file := stickerSetFileWithBytes(t, s, owner, data)
	set, err := createOneFileStickerSet(ctx, s, owner, file, "Revalidate", "revalidate", data)
	if err != nil {
		t.Fatalf("create sticker set: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, `
		UPDATE user_created_sticker_set_items
		SET content_sha256 = NULL
		WHERE set_id = $1 AND file_id = $2
	`, set.ID, file.ID); err != nil {
		t.Fatalf("clear fixture fingerprint: %v", err)
	}
	entered, release := blobs.arm(blob.Key(file.ID))
	var releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRead()

	ref := stickerSetAdminRefByID(t, set.ID)
	takedownDone := make(chan error, 1)
	go func() {
		takedownDone <- s.TakedownUserStickerSet(ctx, ref, "test", make([]byte, sha256.Size))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("takedown did not reach the gated blob read")
	}

	mutationDone := make(chan error, 1)
	go func() {
		_, mutationErr := store.StorePool(s).Exec(ctx, `
			UPDATE user_created_sticker_sets SET item_count = 2 WHERE id = $1
		`, set.ID)
		mutationDone <- mutationErr
	}()
	select {
	case mutationErr := <-mutationDone:
		if mutationErr != nil {
			releaseRead()
			takedownErr := <-takedownDone
			t.Fatalf("mutate set during pre-read: %v (takedown: %v)", mutationErr, takedownErr)
		}
	case <-time.After(3 * time.Second):
		releaseRead()
		takedownErr := <-takedownDone
		mutationErr := <-mutationDone
		t.Fatalf("set remained locked during blob read (takedown: %v, mutation: %v)", takedownErr, mutationErr)
	}

	releaseRead()
	if err := <-takedownDone; !errors.Is(err, store.ErrStickerSetStateConflict) {
		t.Fatalf("takedown after set changed during blob read: %v, want ErrStickerSetStateConflict", err)
	}

	var isLive bool
	var contentSHA256 []byte
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT s.is_live, i.content_sha256
		FROM user_created_sticker_sets s
		JOIN user_created_sticker_set_items i ON i.set_id = s.id
		WHERE s.id = $1 AND i.file_id = $2
	`, set.ID, file.ID).Scan(&isLive, &contentSHA256); err != nil {
		t.Fatalf("read set after stale takedown: %v", err)
	}
	if !isLive || contentSHA256 != nil {
		t.Fatalf("set after stale takedown is_live=%t content_sha256=%x, want live with no published fingerprint", isLive, contentSHA256)
	}
	var events int
	if err := store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM admin_sticker_set_events WHERE set_id = $1`, set.ID).Scan(&events); err != nil {
		t.Fatalf("count takedown audit events: %v", err)
	}
	if events != 0 {
		t.Fatalf("audit events after stale takedown = %d, want 0", events)
	}
}

func TestStickerSetTakedownWaitsForInFlightCreateAndBlocksLaterDuplicate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	blobs := &stickerSetReadGate{Store: testBlobs(t)}
	dsn := pgtest.DSN(t)
	s := openStickerSetReadGateStore(t, blobs, dsn)
	owner := mustUser(t, s, "+15559310021")
	creatingOwner := mustUser(t, s, "+15559310022")
	setData := []byte("hello world")
	setFile := stickerSetFileWithBytes(t, s, owner, setData)
	set, err := createOneFileStickerSet(ctx, s, owner, setFile, "Original", "original_set", setData)
	if err != nil {
		t.Fatalf("create original sticker set: %v", err)
	}
	creatingFile := stickerSetFileWithBytes(t, s, creatingOwner, setData)
	laterFile := stickerSetFileWithBytes(t, s, creatingOwner, setData)

	const gateKey1 int32 = 1145132188
	const gateKey2 int32 = 237731252
	if _, err := store.StorePool(s).Exec(ctx, `
		CREATE FUNCTION test_sticker_set_create_gate() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(1145132188, 237731252);
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER test_sticker_set_create_gate
		BEFORE INSERT ON user_created_sticker_sets
		FOR EACH ROW EXECUTE FUNCTION test_sticker_set_create_gate();
	`); err != nil {
		t.Fatalf("install sticker-set create gate: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.StorePool(s).Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_sticker_set_create_gate ON user_created_sticker_sets;
			DROP FUNCTION IF EXISTS test_sticker_set_create_gate();
		`); err != nil {
			t.Errorf("remove sticker-set create gate: %v", err)
		}
	})

	control, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect sticker-set gate: %v", err)
	}
	t.Cleanup(func() {
		if err := control.Close(context.Background()); err != nil {
			t.Errorf("close sticker-set gate: %v", err)
		}
	})
	gateTx, err := control.Begin(ctx)
	if err != nil {
		t.Fatalf("begin sticker-set gate: %v", err)
	}
	defer func() { _ = gateTx.Rollback(context.Background()) }() //nolint:errcheck // no-op after release
	if _, err := gateTx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, gateKey1, gateKey2); err != nil {
		t.Fatalf("hold sticker-set create gate: %v", err)
	}

	createDone := make(chan error, 1)
	go func() {
		_, createErr := createOneFileStickerSet(ctx, s, creatingOwner, creatingFile, "Concurrent", "concurrent_same", setData)
		createDone <- createErr
	}()
	if err := waitForStickerSetCreateGate(ctx, control); err != nil {
		t.Fatal(err)
	}

	ref := stickerSetAdminRefByID(t, set.ID)
	takedownDone := make(chan error, 1)
	go func() {
		takedownDone <- s.TakedownUserStickerSet(ctx, ref, "test", make([]byte, sha256.Size))
	}()
	if err := waitForStickerTakedownInterlockWaiter(ctx, control); err != nil {
		t.Fatal(err)
	}

	if err := gateTx.Commit(ctx); err != nil {
		t.Fatalf("release sticker-set create gate: %v", err)
	}
	select {
	case createErr := <-createDone:
		if createErr != nil {
			t.Fatalf("finish in-flight create: %v", createErr)
		}
	case <-ctx.Done():
		t.Fatalf("in-flight create did not finish: %v", ctx.Err())
	}
	select {
	case takedownErr := <-takedownDone:
		if takedownErr != nil {
			t.Fatalf("takedown after in-flight create: %v", takedownErr)
		}
	case <-ctx.Done():
		t.Fatalf("takedown did not finish after create: %v", ctx.Err())
	}

	if _, err := createOneFileStickerSet(ctx, s, creatingOwner, laterFile, "Later", "later_same", setData); !errors.Is(err, store.ErrStickerFileBlocked) {
		t.Fatalf("create after takedown = %v, want ErrStickerFileBlocked", err)
	}
}

func waitForStickerSetCreateGate(ctx context.Context, conn *pgx.Conn) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks
				WHERE locktype = 'advisory'
				  AND classid = 1145132188::oid
				  AND objid = 237731252::oid
				  AND objsubid = 2
				  AND NOT granted
			)
		`).Scan(&waiting)
		if err != nil {
			return fmt.Errorf("observe create gate waiter: %w", err)
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("create did not reach gate: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForStickerTakedownInterlockWaiter(ctx context.Context, conn *pgx.Conn) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_locks
				WHERE locktype = 'advisory'
				  AND classid = 1145132116::oid
				  AND objid = 237731251::oid
				  AND objsubid = 2
				  AND NOT granted
			)
		`).Scan(&waiting)
		if err != nil {
			return fmt.Errorf("observe takedown interlock waiter: %w", err)
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("takedown did not queue on interlock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
