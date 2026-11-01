package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

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

func openStickerSetReadGateStore(t *testing.T, blobs *stickerSetReadGate) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(blobs))
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
	digest := sha256.Sum256(data)
	return s.CreateUserStickerSet(ctx, store.UserStickerSet{
		CreatorID: owner.ID, Title: title, ShortName: shortName, Kind: "regular",
	}, []store.UserStickerSetItem{{
		FileID: file.ID, AccessHash: file.AccessHash, ContentSHA256: digest[:], Size: file.Size,
		Emoji: "⭐", Format: "WEBP", Width: 512, Height: 512,
	}})
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

	takedownDone := make(chan error, 1)
	go func() {
		takedownDone <- s.TakedownUserStickerSet(ctx, store.StickerSetAdminRef{ID: set.ID}, "test", make([]byte, sha256.Size))
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
	s := openStickerSetReadGateStore(t, blobs)
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

	takedownDone := make(chan error, 1)
	go func() {
		takedownDone <- s.TakedownUserStickerSet(ctx, store.StickerSetAdminRef{ID: set.ID}, "test", make([]byte, sha256.Size))
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
