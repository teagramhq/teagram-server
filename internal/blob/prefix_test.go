package blob_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
)

type prefixWalkProbe struct {
	blob.Store

	calls int
}

func (p *prefixWalkProbe) WalkPrefix(context.Context, string, func(blob.Entry) error) error {
	p.calls++
	return errors.New("backend WalkPrefix called")
}

func TestPrefixedStoreWalkPrefixRejectsInvalidPrefixesBeforeBackendCall(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"/", "parts//", "parts/../"} {
		t.Run(prefix, func(t *testing.T) {
			backend := &prefixWalkProbe{}
			prefixed, err := blob.NewPrefixedStore(backend, blob.EncryptedObjectsPrefix)
			if err != nil {
				t.Fatalf("create prefixed store: %v", err)
			}

			err = prefixed.WalkPrefix(context.Background(), prefix, func(blob.Entry) error { return nil })
			if !errors.Is(err, blob.ErrInvalidKey) {
				t.Fatalf("walk invalid prefix %q: err = %v, want ErrInvalidKey", prefix, err)
			}
			if backend.calls != 0 {
				t.Fatalf("backend WalkPrefix called %d times for invalid prefix %q", backend.calls, prefix)
			}
		})
	}
}

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
