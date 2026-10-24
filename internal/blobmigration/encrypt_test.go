package blobmigration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmigration"
)

func TestEncryptReferencedResumesWithoutRewritingVerifiedObjects(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	partKey, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("new part key: %v", err)
	}
	plain := map[string][]byte{
		blob.Key(1):   []byte("assembled media"),
		blob.Key(2):   []byte("another file"),
		partKey:       []byte("in-flight part"),
		blob.Key(999): []byte("unreferenced stray"),
	}
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: cloneObjects(plain)},
		putCounts:   make(map[string]int),
		afterPut: func(key string) {
			if strings.HasPrefix(key, blob.EncryptedObjectsPrefix+"/") {
				cancel()
			}
		},
	}
	ring := migrationTestKeyRing(t)
	refs := []blobinventory.Reference{
		{Key: blob.Key(2), Kind: "file", ID: "2", LogicalBytes: int64(len(plain[blob.Key(2)]))},
		{Key: partKey, Kind: "part", ID: partKey, LogicalBytes: int64(len(plain[partKey]))},
		{Key: blob.Key(1), Kind: "file", ID: "1", LogicalBytes: int64(len(plain[blob.Key(1)]))},
	}
	progress := t.TempDir() + "/migration-progress.json"
	options := blobmigration.EncryptionOptions{ProgressPath: progress, Writes: "stopped"}

	if _, err := blobmigration.EncryptReferenced(ctx, store, ring, refs, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted migration error = %v, want context cancellation", err)
	}
	for key, want := range plain {
		if !bytes.Equal(store.objects[key], want) {
			t.Fatalf("plaintext object %q changed during interrupted pass", key)
		}
	}

	store.afterPut = nil
	summary, err := blobmigration.EncryptReferenced(context.Background(), store, ring, refs, options)
	if err != nil {
		t.Fatalf("resume migration: %v", err)
	}
	if summary.Objects != 3 || summary.Bytes != int64(len(plain[blob.Key(1)])+len(plain[blob.Key(2)])+len(plain[partKey])) {
		t.Fatalf("summary = %+v, want three references and their logical bytes", summary)
	}
	if summary.AlreadyVerified != 1 {
		t.Fatalf("already-verified objects = %d, want the interrupted object reused", summary.AlreadyVerified)
	}
	if summary.Writes != "stopped" || summary.InventorySHA256 == "" || summary.ProgressSHA256 == "" {
		t.Fatalf("summary evidence = %+v, want recorded write choice and inventory/progress digests", summary)
	}
	inventoryDigest := sha256.Sum256(summary.InventoryReport)
	if got := hex.EncodeToString(inventoryDigest[:]); got != summary.InventorySHA256 {
		t.Fatalf("inventory report digest = %s, want %s", got, summary.InventorySHA256)
	}
	var savedInventory struct {
		Schema    string               `json:"schema"`
		Encrypted blobinventory.Report `json:"encrypted"`
	}
	if err := json.Unmarshal(summary.InventoryReport, &savedInventory); err != nil {
		t.Fatalf("decode saved inventory report: %v", err)
	}
	if savedInventory.Schema != "teagram.blob-encryption-inventory/v1" ||
		savedInventory.Encrypted.Classes.ValidEnvelope.Count != 3 || savedInventory.Encrypted.Classes.NoValidEnvelope.Count != 0 ||
		savedInventory.Encrypted.Classes.Missing.Count != 0 {
		t.Fatalf("saved encrypted inventory = %+v, want all three references valid", savedInventory)
	}
	if store.putCounts[blob.EncryptedObjectsPrefix+"/"+blob.Key(1)] != 1 {
		t.Fatalf("first encrypted object writes = %d, want one write across both runs", store.putCounts[blob.EncryptedObjectsPrefix+"/"+blob.Key(1)])
	}
	if !bytes.Equal(store.objects[blob.Key(999)], plain[blob.Key(999)]) {
		t.Fatal("unreferenced object was changed")
	}
	if _, exists := store.objects[blob.EncryptedObjectsPrefix+"/"+blob.Key(999)]; exists {
		t.Fatal("unreferenced object received an encrypted copy")
	}
	for key, want := range plain {
		if !bytes.Equal(store.objects[key], want) {
			t.Fatalf("plaintext copy %q was deleted or changed without cleanup", key)
		}
	}
	if !containsObjectID(summary.Unaccounted, "999") {
		t.Fatalf("unaccounted inventory = %+v, want stray file id 999 reported", summary.Unaccounted)
	}

	prefixed, err := blob.NewPrefixedStore(store, blob.EncryptedObjectsPrefix)
	if err != nil {
		t.Fatalf("create prefixed store: %v", err)
	}
	inventory, err := blobinventory.Scan(context.Background(), prefixed, ring, refs)
	if err != nil {
		t.Fatalf("scan migrated objects: %v", err)
	}
	if inventory.Classes.ValidEnvelope.Count != 3 || inventory.Classes.NoValidEnvelope.Count != 0 || inventory.Classes.Missing.Count != 0 {
		t.Fatalf("post-migration inventory = %+v, want every reference encrypted", inventory.Classes)
	}
}

func TestEncryptReferencedRejectsPlaintextChangedAfterCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := blob.Key(7)
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: map[string][]byte{key: []byte("media")}},
		putCounts:   make(map[string]int),
	}
	store.afterPut = func(physicalKey string) {
		if strings.HasPrefix(physicalKey, blob.EncryptedObjectsPrefix+"/") {
			store.objects[key] = []byte("other")
		}
	}
	ring := migrationTestKeyRing(t)
	ref := blobinventory.Reference{Key: key, Kind: "file", ID: "7", LogicalBytes: 5}
	_, err := blobmigration.EncryptReferenced(ctx, store, ring, []blobinventory.Reference{ref}, blobmigration.EncryptionOptions{
		ProgressPath: t.TempDir() + "/migration-progress.json", Writes: "stopped",
	})
	if err == nil || !strings.Contains(err.Error(), "plaintext source changed during migration") {
		t.Fatalf("migration error = %v, want changed plaintext source rejection", err)
	}
}

func TestCleanupPlaintextRemovesOnlyVerifiedReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	strayKey := blob.Key(99)
	refs := []blobinventory.Reference{{Key: blob.Key(8), Kind: "file", ID: "8", LogicalBytes: 5}}
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: map[string][]byte{blob.Key(8): []byte("media"), strayKey: []byte("stray")}},
		putCounts:   make(map[string]int),
	}
	options := blobmigration.EncryptionOptions{ProgressPath: t.TempDir() + "/migration-progress.json", Writes: "drained"}
	ring := migrationTestKeyRing(t)
	if _, err := blobmigration.EncryptReferenced(ctx, store, ring, refs, options); err != nil {
		t.Fatalf("migrate referenced object: %v", err)
	}
	summary, err := blobmigration.CleanupPlaintext(ctx, store, ring, refs, options)
	if err != nil {
		t.Fatalf("clean up verified plaintext: %v", err)
	}
	if summary.ObjectsRemoved != 1 || summary.BytesRemoved != 5 {
		t.Fatalf("cleanup summary = %+v, want one 5-byte referenced copy removed", summary)
	}
	if _, exists := store.objects[blob.Key(8)]; exists {
		t.Fatal("verified plaintext copy remains after cleanup")
	}
	if string(store.objects[strayKey]) != "stray" {
		t.Fatal("cleanup touched an unreferenced object")
	}
	if _, exists := store.objects[blob.EncryptedObjectsPrefix+"/"+blob.Key(8)]; !exists {
		t.Fatal("cleanup removed the verified encrypted copy")
	}
}

func TestCleanupPlaintextAllowsNewEncryptedReferencesAfterCutover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	oldKey := blob.Key(8)
	newKey := blob.Key(9)
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: map[string][]byte{oldKey: []byte("older media")}},
		putCounts:   make(map[string]int),
	}
	options := blobmigration.EncryptionOptions{ProgressPath: t.TempDir() + "/migration-progress.json", Writes: "drained"}
	ring := migrationTestKeyRing(t)
	oldRef := blobinventory.Reference{Key: oldKey, Kind: "file", ID: "8", LogicalBytes: 11}
	if _, err := blobmigration.EncryptReferenced(ctx, store, ring, []blobinventory.Reference{oldRef}, options); err != nil {
		t.Fatalf("migrate pre-cutover reference: %v", err)
	}
	prefixed, err := blob.NewPrefixedStore(store, blob.EncryptedObjectsPrefix)
	if err != nil {
		t.Fatalf("create prefixed store: %v", err)
	}
	sealed, err := blob.NewEncrypted(prefixed, ring, blob.DefaultChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	if _, err := sealed.Put(ctx, newKey, strings.NewReader("new media")); err != nil {
		t.Fatalf("write post-cutover encrypted reference: %v", err)
	}
	newRef := blobinventory.Reference{Key: newKey, Kind: "file", ID: "9", LogicalBytes: 9}
	summary, err := blobmigration.CleanupPlaintext(ctx, store, ring, []blobinventory.Reference{oldRef, newRef}, options)
	if err != nil {
		t.Fatalf("clean up after encrypted writes resumed: %v", err)
	}
	if summary.ObjectsRemoved != 1 || summary.BytesRemoved != 11 {
		t.Fatalf("cleanup summary = %+v, want only the old plaintext copy removed", summary)
	}
	if _, exists := store.objects[oldKey]; exists {
		t.Fatal("verified pre-cutover plaintext remains after cleanup")
	}
	if _, exists := store.objects[newKey]; exists {
		t.Fatal("cleanup removed plaintext for a post-cutover reference")
	}
}

func TestCleanupPlaintextRequiresVerifiedEncryptedCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := blob.Key(3)
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: map[string][]byte{key: []byte("media")}},
		putCounts:   make(map[string]int),
	}
	ref := blobinventory.Reference{Key: key, Kind: "file", ID: "3", LogicalBytes: 5}
	ring := migrationTestKeyRing(t)
	progress := blobmigration.EncryptionOptions{ProgressPath: t.TempDir() + "/migration-progress.json", Writes: "stopped"}
	if _, err := blobmigration.EncryptReferenced(ctx, store, ring, []blobinventory.Reference{ref}, progress); err != nil {
		t.Fatalf("migrate before cleanup: %v", err)
	}
	if _, err := store.Put(ctx, blob.EncryptedObjectsPrefix+"/"+key, strings.NewReader("not an envelope")); err != nil {
		t.Fatalf("corrupt encrypted copy fixture: %v", err)
	}
	if _, err := blobmigration.CleanupPlaintext(ctx, store, ring, []blobinventory.Reference{ref}, progress); err == nil {
		t.Fatal("cleanup accepted an invalid encrypted copy")
	}
	if string(store.objects[key]) != "media" {
		t.Fatal("cleanup removed plaintext without a verified encrypted copy")
	}
}

func cloneObjects(objects map[string][]byte) map[string][]byte {
	cloned := make(map[string][]byte, len(objects))
	for key, value := range objects {
		cloned[key] = append([]byte(nil), value...)
	}
	return cloned
}

func migrationTestKeyRing(t *testing.T) *blob.KeyRing {
	t.Helper()
	ring, err := blob.NewKeyRing(4, map[uint32][]byte{4: bytes.Repeat([]byte{0x44}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("new key ring: %v", err)
	}
	return ring
}

func containsObjectID(ids []blobinventory.ObjectID, id string) bool {
	for _, object := range ids {
		if object.ID == id {
			return true
		}
	}
	return false
}

type interruptingMemoryStore struct {
	*memoryStore

	putCounts map[string]int
	afterPut  func(string)
}

func (s *interruptingMemoryStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	n, err := s.memoryStore.Put(ctx, key, r)
	if err != nil {
		return n, err
	}
	s.putCounts[key]++
	if s.afterPut != nil {
		s.afterPut(key)
	}
	return n, nil
}
