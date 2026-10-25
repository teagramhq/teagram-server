package blobinventory_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
)

func TestScanClassifiesReferencedAndUnaccountedObjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x61}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(backend, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	partKey, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("create part key: %v", err)
	}
	if _, err := sealed.Put(ctx, blob.Key(1), bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatalf("put envelope file: %v", err)
	}
	if _, err := backend.Put(ctx, blob.Key(2), bytes.NewReader([]byte("plain"))); err != nil {
		t.Fatalf("put plaintext file: %v", err)
	}
	if _, err := sealed.Put(ctx, partKey, bytes.NewReader([]byte("part"))); err != nil {
		t.Fatalf("put envelope part: %v", err)
	}
	if _, err := backend.Put(ctx, blob.Key(999), bytes.NewReader([]byte("stray"))); err != nil {
		t.Fatalf("put unaccounted object: %v", err)
	}

	report, err := blobinventory.Scan(ctx, backend, ring, []blobinventory.Reference{
		{Key: blob.Key(1), Kind: "file", ID: "1", LogicalBytes: 6},
		{Key: blob.Key(2), Kind: "file", ID: "2", LogicalBytes: 5},
		{Key: partKey, Kind: "part", ID: partKey, LogicalBytes: 4},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Classes.ValidEnvelope.Count != 2 || report.Classes.ValidEnvelope.LogicalBytes != 10 || report.Classes.ValidEnvelope.PhysicalBytes != 246 {
		t.Fatalf("valid envelopes = %+v, want 2 objects, 10 logical bytes, 246 physical bytes", report.Classes.ValidEnvelope)
	}
	if report.Classes.NoValidEnvelope.Count != 1 || report.Classes.NoValidEnvelope.LogicalBytes != 5 || report.Classes.NoValidEnvelope.PhysicalBytes != 5 {
		t.Fatalf("no-valid-envelope = %+v, want one 5-byte plaintext object", report.Classes.NoValidEnvelope)
	}
	if report.Classes.Missing.Count != 0 {
		t.Fatalf("missing = %+v, want none", report.Classes.Missing)
	}
	if report.Classes.Unaccounted.Count != 1 || report.Classes.Unaccounted.LogicalBytes != 5 || report.Classes.Unaccounted.PhysicalBytes != 5 {
		t.Fatalf("unaccounted = %+v, want one 5-byte object", report.Classes.Unaccounted)
	}
	if len(report.Classes.Unaccounted.IDs) != 1 || report.Classes.Unaccounted.IDs[0].ID != "999" {
		t.Fatalf("unaccounted ids = %+v, want file id 999", report.Classes.Unaccounted.IDs)
	}
	if len(report.KeyVersions) != 1 || report.KeyVersions[0].Version != 1 || report.KeyVersions[0].Objects != 2 || report.KeyVersions[0].ReferencedObjects != 2 || report.KeyVersions[0].Retirable {
		t.Fatalf("key versions = %+v, want version 1 with two references and not retirable", report.KeyVersions)
	}
	if report.KeyVersions[0].LogicalBytes != 10 || report.KeyVersions[0].PhysicalBytes != 246 {
		t.Fatalf("version 1 bytes = logical %d, physical %d; want 10 and 246", report.KeyVersions[0].LogicalBytes, report.KeyVersions[0].PhysicalBytes)
	}
	if report.Summary == "" {
		t.Fatal("report has no human summary")
	}
}

func TestScanReportsMissingReferencesAndOnlyRetiresUnreferencedVersions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	keys := map[uint32][]byte{
		1: bytes.Repeat([]byte{0x71}, blob.MediaKeyLen),
		2: bytes.Repeat([]byte{0x72}, blob.MediaKeyLen),
	}
	ring1, err := blob.NewKeyRing(1, keys)
	if err != nil {
		t.Fatalf("create version 1 key ring: %v", err)
	}
	ring2, err := blob.NewKeyRing(2, keys)
	if err != nil {
		t.Fatalf("create version 2 key ring: %v", err)
	}
	oldVersion, err := blob.NewEncrypted(backend, ring1, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	if _, err := oldVersion.Put(ctx, blob.Key(1), bytes.NewReader([]byte("old"))); err != nil {
		t.Fatalf("put old-version envelope: %v", err)
	}
	// An object under version 2 exists, but no database row references it.
	// It must not prevent retiring version 2.
	currentVersion, err := blob.NewEncrypted(backend, ring2, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create current encrypted store: %v", err)
	}
	if _, err := currentVersion.Put(ctx, blob.Key(2), bytes.NewReader([]byte("stray"))); err != nil {
		t.Fatalf("put unreferenced envelope: %v", err)
	}

	report, err := blobinventory.Scan(ctx, backend, ring2, []blobinventory.Reference{
		{Key: blob.Key(1), Kind: "file", ID: "1", LogicalBytes: 3},
		{Key: blob.Key(3), Kind: "file", ID: "3", LogicalBytes: 8},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Classes.Missing.Count != 1 || report.Classes.Missing.LogicalBytes != 8 || report.Classes.Missing.PhysicalBytes != 0 {
		t.Fatalf("missing = %+v, want one 8-byte logical reference and no physical bytes", report.Classes.Missing)
	}
	if len(report.Classes.Missing.IDs) != 1 || report.Classes.Missing.IDs[0].ID != "3" {
		t.Fatalf("missing ids = %+v, want file id 3", report.Classes.Missing.IDs)
	}
	versions := map[uint32]blobinventory.KeyVersion{}
	for _, version := range report.KeyVersions {
		versions[version.Version] = version
	}
	if versions[1].Retirable || versions[1].ReferencedObjects != 1 {
		t.Fatalf("version 1 = %+v, want a live reference and not retirable", versions[1])
	}
	if !versions[2].Retirable || versions[2].ReferencedObjects != 0 {
		t.Fatalf("version 2 = %+v, want zero references and retirable", versions[2])
	}
}

func TestScanRejectsEnvelopeWithLogicalLengthDifferentFromReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x73}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(backend, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	if _, err := sealed.Put(ctx, blob.Key(7), bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatalf("put envelope: %v", err)
	}

	report, err := blobinventory.Scan(ctx, backend, ring, []blobinventory.Reference{{
		Key: blob.Key(7), Kind: "file", ID: "7", LogicalBytes: 5,
	}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Classes.NoValidEnvelope.Count != 1 || report.Classes.ValidEnvelope.Count != 0 {
		t.Fatalf("classes = %+v, want the length-mismatched envelope rejected", report.Classes)
	}
	if len(report.Classes.NoValidEnvelope.IDs) != 1 || report.Classes.NoValidEnvelope.IDs[0].ID != "7" {
		t.Fatalf("remaining IDs = %+v, want file id 7", report.Classes.NoValidEnvelope.IDs)
	}
}

func TestScanFailsWhenTreeListingIsIncomplete(t *testing.T) {
	t.Parallel()
	want := errors.New("listing failed")
	backend := scanFailureStore{listErr: want}
	_, err := blobinventory.Scan(context.Background(), backend, nil, nil)
	if !errors.Is(err, want) {
		t.Fatalf("scan error = %v, want listing failure", err)
	}
}

func TestScanFailsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := scanFailureStore{}
	_, err := blobinventory.Scan(ctx, backend, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context cancellation", err)
	}
}

func TestScanDoesNotReportUnversionedEnvelopeReferenceAsRetirable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x81}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	unknownFormat := make([]byte, 118)
	copy(unknownFormat, "TGME")
	unknownFormat[5] = 2
	if _, err := backend.Put(ctx, blob.Key(1), bytes.NewReader(unknownFormat)); err != nil {
		t.Fatalf("put unknown-format object: %v", err)
	}

	report, err := blobinventory.Scan(ctx, backend, ring, []blobinventory.Reference{{
		Key: blob.Key(1), Kind: "file", ID: "1", LogicalBytes: 102,
	}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Classes.NoValidEnvelope.Count != 1 || report.UnversionedEnvelopeReferences != 1 {
		t.Fatalf("report = %+v, want one invalid reference with an unreadable version", report)
	}
	if report.KeyVersions[0].Retirable {
		t.Fatalf("version 1 = %+v, want retirement blocked by the unreadable referenced header", report.KeyVersions[0])
	}
}

func TestScanDoesNotRetireVersionClaimedByTruncatedReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(6, map[uint32][]byte{6: bytes.Repeat([]byte{0x86}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(backend, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	const key = "06/6"
	if _, err := sealed.Put(ctx, key, bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatalf("put envelope: %v", err)
	}
	header, err := backend.ReadAt(ctx, key, 0, 102)
	if err != nil {
		t.Fatalf("read source header: %v", err)
	}
	if _, err := backend.Put(ctx, key, bytes.NewReader(header)); err != nil {
		t.Fatalf("replace with truncated envelope: %v", err)
	}

	report, err := blobinventory.Scan(ctx, backend, ring, []blobinventory.Reference{{
		Key: key, Kind: "file", ID: "6", LogicalBytes: 6,
	}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Classes.NoValidEnvelope.Count != 1 {
		t.Fatalf("no-valid-envelope = %+v, want the truncated reference", report.Classes.NoValidEnvelope)
	}
	if len(report.KeyVersions) != 1 || report.KeyVersions[0].Version != 6 || report.KeyVersions[0].ReferencedObjects != 1 || report.KeyVersions[0].Retirable {
		t.Fatalf("key versions = %+v, want version 6 blocked by the truncated reference", report.KeyVersions)
	}
}

func TestScanFailsWhenEnvelopeKeyRingIsUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(4, map[uint32][]byte{4: bytes.Repeat([]byte{0x84}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(backend, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	if _, err := sealed.Put(ctx, blob.Key(4), bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatalf("put envelope: %v", err)
	}
	_, err = blobinventory.Scan(ctx, backend, nil, []blobinventory.Reference{{
		Key: blob.Key(4), Kind: "file", ID: "4", LogicalBytes: 6,
	}})
	if !errors.Is(err, blob.ErrEnvelopeKeyUnavailable) {
		t.Fatalf("scan error = %v, want ErrEnvelopeKeyUnavailable", err)
	}
}

type scanFailureStore struct {
	listErr error
}

func (s scanFailureStore) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("unexpected Put")
}

func (s scanFailureStore) ReadAt(context.Context, string, int64, int64) ([]byte, error) {
	return nil, errors.New("unexpected ReadAt")
}

func (s scanFailureStore) Remove(context.Context, string) error {
	return errors.New("unexpected Remove")
}

func (s scanFailureStore) WalkPrefix(context.Context, string, func(blob.Entry) error) error {
	return errors.New("unexpected WalkPrefix")
}

func (s scanFailureStore) Walk(context.Context, func(blob.Entry) error) error {
	return s.listErr
}
