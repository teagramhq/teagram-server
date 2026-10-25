package blob_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
)

func TestPrefixedStoreKeepsLogicalEnvelopeIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	prefixed, err := blob.NewPrefixedStore(backend, "encrypted")
	if err != nil {
		t.Fatalf("create prefixed store: %v", err)
	}
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x31}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(prefixed, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	if _, err := sealed.Put(ctx, "02/1", bytes.NewReader([]byte("media"))); err != nil {
		t.Fatalf("put encrypted object: %v", err)
	}
	physical, err := backend.ReadAt(ctx, "encrypted/02/1", 0, 4)
	if err != nil || string(physical) != "TGME" {
		t.Fatalf("physical object prefix = %q, %v; want TGME", physical, err)
	}
	if _, err := backend.ReadAt(ctx, "02/1", 0, 1); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("logical key unexpectedly exists in the raw backend: %v", err)
	}
	got, err := sealed.ReadAt(ctx, "02/1", 0, 5)
	if err != nil || string(got) != "media" {
		t.Fatalf("read encrypted object = %q, %v; want media", got, err)
	}

	var keys []string
	if err := sealed.WalkPrefix(ctx, "02", func(entry blob.Entry) error {
		if entry.Regular {
			keys = append(keys, entry.Key)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk encrypted objects: %v", err)
	}
	if len(keys) != 1 || keys[0] != "02/1" {
		t.Fatalf("walk keys = %v, want [02/1]", keys)
	}
	if _, err := blobinventory.Scan(ctx, prefixed, ring, []blobinventory.Reference{{
		Key: "02/1", Kind: "file", ID: "1", LogicalBytes: 5,
	}}); err != nil {
		t.Fatalf("inventory prefixed encrypted object: %v", err)
	}
}
