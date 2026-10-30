package blobmigration_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmigration"
)

func TestRotateKeyVersionMovesFilesAndLivePartsToCurrentKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: make(map[string][]byte)},
		putCounts:   make(map[string]int),
	}
	oldRing := rotationKeyRing(t, 1)
	currentRing := rotationKeyRing(t, 2)
	oldWriter, err := blob.NewEncrypted(store, oldRing, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create version 1 writer: %v", err)
	}
	partKey, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("new part key: %v", err)
	}
	objects := map[string][]byte{
		blob.Key(1): []byte("first assembled file"),
		blob.Key(2): bytes.Repeat([]byte("second"), 900),
		blob.Key(3): []byte("third assembled file"),
		partKey:     []byte("live upload part"),
	}
	refs := make([]blobinventory.Reference, 0, len(objects))
	for index, key := range []string{blob.Key(1), blob.Key(2), blob.Key(3), partKey} {
		body := objects[key]
		if _, err := oldWriter.Put(ctx, key, bytes.NewReader(body)); err != nil {
			t.Fatalf("write version 1 object %q: %v", key, err)
		}
		kind, id := "file", string(rune('1'+index))
		if key == partKey {
			kind, id = "part", key
		}
		refs = append(refs, blobinventory.Reference{Key: key, Kind: kind, ID: id, LogicalBytes: int64(len(body))})
	}
	clear(store.putCounts)

	summary, err := blobmigration.RotateKeyVersion(ctx, store, currentRing, refs, blobmigration.RotationOptions{
		ProgressPath: t.TempDir() + "/rotation-progress.jsonl",
	})
	if err != nil {
		t.Fatalf("rotate referenced objects: %v", err)
	}
	if summary.Objects != 4 || summary.Rotated != 4 || summary.AlreadyVerified != 0 {
		t.Fatalf("rotation summary = %+v, want four rotated objects", summary)
	}
	currentOnlyRing, err := blob.NewKeyRing(2, map[uint32][]byte{
		2: bytes.Repeat([]byte{0x42}, blob.MediaKeyLen),
	})
	if err != nil {
		t.Fatalf("create current-only key ring: %v", err)
	}
	reader, err := blob.NewEncrypted(store, currentOnlyRing, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create current reader: %v", err)
	}
	for key, want := range objects {
		got, err := reader.ReadAt(ctx, key, 0, int64(len(want)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("read rotated object %q = %q, %v; want original bytes", key, got, err)
		}
	}
	report, err := blobinventory.Scan(ctx, store, currentRing, refs)
	if err != nil {
		t.Fatalf("inventory after rotation: %v", err)
	}
	versions := make(map[uint32]blobinventory.KeyVersion, len(report.KeyVersions))
	for _, version := range report.KeyVersions {
		versions[version.Version] = version
	}
	if versions[1].Objects != 0 || versions[1].ReferencedObjects != 0 || !versions[1].Retirable ||
		versions[2].Objects != 4 || versions[2].ReferencedObjects != 4 {
		t.Fatalf("key versions after rotation = %+v, want 1 retirable and all four references at 2", report.KeyVersions)
	}
}

func TestRotateKeyVersionResumesPreparedObjectWithoutWritingItTwice(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: make(map[string][]byte)},
		putCounts:   make(map[string]int),
	}
	oldRing := rotationKeyRing(t, 1)
	currentRing := rotationKeyRing(t, 2)
	oldWriter, err := blob.NewEncrypted(store, oldRing, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create version 1 writer: %v", err)
	}
	wants := [][]byte{[]byte("eleven"), []byte("twelve!")}
	refs := []blobinventory.Reference{
		{Key: blob.Key(11), Kind: "file", ID: "11", LogicalBytes: int64(len(wants[0]))},
		{Key: blob.Key(12), Kind: "file", ID: "12", LogicalBytes: int64(len(wants[1]))},
	}
	for index, ref := range refs {
		if _, err := oldWriter.Put(ctx, ref.Key, bytes.NewReader(wants[index])); err != nil {
			t.Fatalf("write version 1 object %q: %v", ref.Key, err)
		}
	}
	clear(store.putCounts)
	store.afterPut = func(string) { cancel() }
	progress := t.TempDir() + "/rotation-progress.jsonl"
	options := blobmigration.RotationOptions{ProgressPath: progress}
	if _, err := blobmigration.RotateKeyVersion(ctx, store, currentRing, refs, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted rotation error = %v, want context cancellation", err)
	}
	store.afterPut = nil
	reader, err := blob.NewEncrypted(store, currentRing, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create current reader: %v", err)
	}
	for index, ref := range refs {
		got, err := reader.ReadAt(context.Background(), ref.Key, 0, int64(len(wants[index])))
		if err != nil || !bytes.Equal(got, wants[index]) {
			t.Fatalf("read object %q after interruption = %q, %v", ref.Key, got, err)
		}
	}

	summary, err := blobmigration.RotateKeyVersion(context.Background(), store, currentRing, refs, options)
	if err != nil {
		t.Fatalf("resume rotation: %v", err)
	}
	if summary.Rotated != 1 || summary.AlreadyVerified != 1 {
		t.Fatalf("resumed summary = %+v, want one newly rotated and one recovered", summary)
	}
	if store.putCounts[refs[0].Key] != 1 {
		t.Fatalf("first object writes = %d, want one write across interruption and resume", store.putCounts[refs[0].Key])
	}
}

func TestRotateKeyVersionRerunFindsPartWrittenUnderOldVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: make(map[string][]byte)},
		putCounts:   make(map[string]int),
	}
	oldRing := rotationKeyRing(t, 1)
	currentRing := rotationKeyRing(t, 2)
	oldWriter, err := blob.NewEncrypted(store, oldRing, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create version 1 writer: %v", err)
	}
	first := blobinventory.Reference{Key: blob.Key(21), Kind: "file", ID: "21", LogicalBytes: 5}
	if _, err := oldWriter.Put(ctx, first.Key, bytes.NewReader([]byte("first"))); err != nil {
		t.Fatalf("write initial object: %v", err)
	}
	clear(store.putCounts)
	progress := t.TempDir() + "/rotation-progress.jsonl"
	options := blobmigration.RotationOptions{ProgressPath: progress}
	if _, err := blobmigration.RotateKeyVersion(ctx, store, currentRing, []blobinventory.Reference{first}, options); err != nil {
		t.Fatalf("first rotation pass: %v", err)
	}
	partKey, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("new part key: %v", err)
	}
	part := []byte("part uploaded during the pass")
	if _, err := oldWriter.Put(ctx, partKey, bytes.NewReader(part)); err != nil {
		t.Fatalf("write concurrent old-version part: %v", err)
	}
	refs := []blobinventory.Reference{
		first,
		{Key: partKey, Kind: "part", ID: partKey, LogicalBytes: int64(len(part))},
	}
	summary, err := blobmigration.RotateKeyVersion(ctx, store, currentRing, refs, options)
	if err != nil {
		t.Fatalf("rerun with the new part reference: %v", err)
	}
	if summary.Rotated != 1 || summary.AlreadyVerified != 1 {
		t.Fatalf("rerun summary = %+v, want the new part rotated and the file reused", summary)
	}
	report, err := blobinventory.Scan(ctx, store, currentRing, refs)
	if err != nil {
		t.Fatalf("inventory after rerun: %v", err)
	}
	if report.Classes.ValidEnvelope.Count != 2 || report.KeyVersions[0].ReferencedObjects != 0 || !report.KeyVersions[0].Retirable {
		t.Fatalf("inventory after rerun = %+v, want both references at version 2 and version 1 retirable", report)
	}
}

func TestRewriteModeUsesFreshDEKAndResumeDoesNotRewriteAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &interruptingMemoryStore{
		memoryStore: &memoryStore{objects: make(map[string][]byte)},
		putCounts:   make(map[string]int),
	}
	ring := rotationKeyRing(t, 2)
	writer, err := blob.NewEncrypted(store, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create version 2 writer: %v", err)
	}
	key := blob.Key(31)
	want := bytes.Repeat([]byte("leaked wrapping key"), 700)
	if _, err := writer.Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("write version 2 object: %v", err)
	}
	oldObject := bytes.Clone(store.objects[key])
	clear(store.putCounts)
	ref := blobinventory.Reference{Key: key, Kind: "file", ID: "31", LogicalBytes: int64(len(want))}
	options := blobmigration.RotationOptions{ProgressPath: t.TempDir() + "/rewrite-progress.jsonl", Rewrite: true}
	first, err := blobmigration.RotateKeyVersion(ctx, store, ring, []blobinventory.Reference{ref}, options)
	if err != nil || first.Rotated != 1 {
		t.Fatalf("rewrite leaked-key object = %+v, %v; want one rewrite", first, err)
	}
	if bytes.Equal(oldObject[102:], store.objects[key][102:]) {
		t.Fatal("rewrite mode kept ciphertext created under the leaked data key")
	}
	second, err := blobmigration.RotateKeyVersion(ctx, store, ring, []blobinventory.Reference{ref}, options)
	if err != nil || second.AlreadyVerified != 1 || store.putCounts[key] != 1 {
		t.Fatalf("rewrite resume = %+v, %v, writes=%d; want one verified object and no second write", second, err, store.putCounts[key])
	}
	got, err := writer.ReadAt(ctx, key, 0, int64(len(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read rewritten object = %v; want original bytes", err)
	}
}

func rotationKeyRing(t *testing.T, current uint32) *blob.KeyRing {
	t.Helper()
	ring, err := blob.NewKeyRing(current, map[uint32][]byte{
		1: bytes.Repeat([]byte{0x41}, blob.MediaKeyLen),
		2: bytes.Repeat([]byte{0x42}, blob.MediaKeyLen),
	})
	if err != nil {
		t.Fatalf("create rotation key ring: %v", err)
	}
	return ring
}
