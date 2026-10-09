package blobmigration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobmigration"
)

const testChunkSize = 64 << 10

type summaryReport struct {
	blobmigration.Summary

	Type string `json:"type"`
}

func TestMigrateCopiesAndVerifiesEveryBlob(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sourceDir := t.TempDir()
	source, err := blob.NewLocal(sourceDir)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	large := bytes.Repeat([]byte("blob"), testChunkSize/2+3)
	assembledKey := blob.Key(259)
	if _, err := source.Put(ctx, assembledKey, bytes.NewReader(large)); err != nil {
		t.Fatalf("write assembled source: %v", err)
	}
	partKey, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("new part key: %v", err)
	}
	if _, err := source.Put(ctx, partKey, strings.NewReader("in-flight part")); err != nil {
		t.Fatalf("write upload part source: %v", err)
	}

	destination := &memoryStore{objects: make(map[string][]byte)}
	var report bytes.Buffer
	summary, err := blobmigration.Migrate(ctx, source, destination, &report)
	if err != nil {
		t.Fatalf("migrate source: %v", err)
	}
	if summary.Objects != 2 || summary.Bytes != int64(len(large)+len("in-flight part")) {
		t.Fatalf("summary = %#v, want two objects and exact bytes", summary)
	}
	if !bytes.Equal(destination.objects[assembledKey], large) || string(destination.objects[partKey]) != "in-flight part" {
		t.Fatal("destination contents differ from source")
	}
	if summary.SourceManifestSHA256 == "" || summary.SourceManifestSHA256 != summary.DestinationManifestSHA256 {
		t.Fatalf("manifest checksums = %#v", summary)
	}

	lines := strings.Split(strings.TrimSpace(report.String()), "\n")
	if len(lines) != summary.Objects+1 {
		t.Fatalf("report has %d lines, want %d object records and summary", len(lines), summary.Objects+1)
	}
	var gotSummary summaryReport
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &gotSummary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if gotSummary.Type != "summary" || gotSummary.Summary != summary {
		t.Fatalf("reported summary = %#v, want %#v", gotSummary, summary)
	}

	// An interrupted copy can safely be rerun: same-key puts replace the
	// already-copied object, and the full destination key set remains exact.
	var retryReport bytes.Buffer
	retrySummary, err := blobmigration.Migrate(ctx, source, destination, &retryReport)
	if err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	if retrySummary != summary {
		t.Fatalf("retry summary = %#v, want %#v", retrySummary, summary)
	}
}

func TestMigrateRejectsUnexpectedDestinationKey(t *testing.T) {
	t.Parallel()

	source, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	destination := &memoryStore{objects: map[string][]byte{"foreign/object": []byte("unexpected")}}
	var report bytes.Buffer
	if _, err := blobmigration.Migrate(context.Background(), source, destination, &report); err == nil || !strings.Contains(err.Error(), "unexpected blob") {
		t.Fatalf("migration error = %v, want unexpected destination key", err)
	}
	if report.Len() != 0 {
		t.Fatalf("report emitted before destination namespace validation: %q", report.String())
	}
}

func TestMigrateRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sourceDir := t.TempDir()
	source, err := blob.NewLocal(sourceDir)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	key := blob.Key(7)
	if _, err := source.Put(ctx, key, strings.NewReader("source")); err != nil {
		t.Fatalf("write source: %v", err)
	}
	destination := &memoryStore{objects: make(map[string][]byte), corruptPut: true}
	var report bytes.Buffer
	if _, err := blobmigration.Migrate(ctx, source, destination, &report); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("migration error = %v, want checksum mismatch", err)
	}
	if strings.Contains(report.String(), `"type":"summary"`) {
		t.Fatalf("reported success after checksum mismatch: %q", report.String())
	}
}

func TestMigrateRejectsOversizedPartBlob(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	source, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	key, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("new part key: %v", err)
	}
	oversized := bytes.Repeat([]byte{'x'}, blob.MaxPartBytes+1)
	if _, err := source.Put(ctx, key, bytes.NewReader(oversized)); err != nil {
		t.Fatalf("write oversized source part: %v", err)
	}
	destination := &memoryStore{objects: make(map[string][]byte)}
	if _, err := blobmigration.Migrate(ctx, source, destination, io.Discard); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("migration error = %v, want oversized part rejection", err)
	}
	if len(destination.objects) != 0 {
		t.Fatalf("destination received oversized part: %d objects", len(destination.objects))
	}
}

func TestMigrateRejectsNonRegularSourceEntry(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()
	if err := os.Symlink(filepath.Join(sourceDir, "missing"), filepath.Join(sourceDir, "link")); err != nil {
		t.Fatalf("create source symlink: %v", err)
	}
	source, err := blob.NewLocal(sourceDir)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	if _, err := blobmigration.Migrate(context.Background(), source, &memoryStore{objects: make(map[string][]byte)}, io.Discard); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("migration error = %v, want non-regular source entry", err)
	}
}

func TestCensusWritesCanonicalSortedManifest(t *testing.T) {
	t.Parallel()

	store := &memoryStore{objects: map[string][]byte{
		blob.Key(259): []byte("third"),
		blob.Key(7):   []byte("first"),
	}}
	var manifest bytes.Buffer
	summary, err := blobmigration.Census(context.Background(), store, &manifest)
	if err != nil {
		t.Fatalf("census blobs: %v", err)
	}
	firstSHA := sha256.Sum256([]byte("first"))
	thirdSHA := sha256.Sum256([]byte("third"))
	want := "03/259\t5\t" + hex.EncodeToString(thirdSHA[:]) + "\n" +
		"07/7\t5\t" + hex.EncodeToString(firstSHA[:]) + "\n"
	if manifest.String() != want {
		t.Fatalf("manifest = %q, want %q", manifest.String(), want)
	}
	wantManifestSHA := sha256.Sum256([]byte(want))
	if summary.Objects != 2 || summary.Bytes != 10 || summary.ManifestSHA256 != hex.EncodeToString(wantManifestSHA[:]) {
		t.Fatalf("summary = %#v, want exact object, byte, and manifest totals", summary)
	}
}

func TestVerifiedCopiesWriteCanonicalManifest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	source := &memoryStore{objects: map[string][]byte{
		blob.Key(259): []byte("third"),
		blob.Key(7):   []byte("first"),
	}}
	destination := &memoryStore{objects: make(map[string][]byte)}
	var copyManifest bytes.Buffer
	if _, err := blobmigration.MigrateWithManifest(ctx, mustLocal(t, source), destination, io.Discard, &copyManifest); err != nil {
		t.Fatalf("migrate with manifest: %v", err)
	}
	var destinationManifest bytes.Buffer
	if _, err := blobmigration.Census(ctx, destination, &destinationManifest); err != nil {
		t.Fatalf("census destination: %v", err)
	}
	if copyManifest.String() != destinationManifest.String() {
		t.Fatalf("copy manifest differs from independent destination census\ncopy: %q\ndestination: %q", copyManifest.String(), destinationManifest.String())
	}
}

func mustLocal(t *testing.T, source *memoryStore) *blob.Local {
	t.Helper()
	local, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create local source: %v", err)
	}
	for key, value := range source.objects {
		if _, err := local.Put(context.Background(), key, bytes.NewReader(value)); err != nil {
			t.Fatalf("write local source blob %q: %v", key, err)
		}
	}
	return local
}

func TestMigrateAndCensusRejectTemporaryKeys(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(sourceDir, "01"), 0o700); err != nil {
		t.Fatalf("create source shard: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "01", ".tmp-7"), []byte("unfinished"), 0o600); err != nil {
		t.Fatalf("write temporary source key: %v", err)
	}
	local, err := blob.NewLocal(sourceDir)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	remote := &memoryStore{objects: map[string][]byte{"01/.tmp-7": []byte("unfinished")}}
	for name, operation := range map[string]func() error{
		"migrate": func() error {
			_, err := blobmigration.Migrate(context.Background(), local, remote, io.Discard)
			return err
		},
		"census": func() error {
			_, err := blobmigration.Census(context.Background(), remote, io.Discard)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := operation(); err == nil || !strings.Contains(err.Error(), "temporary blob key") {
				t.Fatalf("operation error = %v, want temporary-key rejection", err)
			}
		})
	}
}

func TestRestoreCopiesNewAndUpdatedBlobsAndPreservesLocalOnlyBlobs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	key := blob.Key(11)
	newKey := blob.Key(12)
	localOnlyKey := blob.Key(13)
	source := &memoryStore{objects: map[string][]byte{
		key:    []byte("updated from S3"),
		newKey: []byte("uploaded after cutover"),
	}}
	destination, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create local destination: %v", err)
	}
	if _, err := destination.Put(ctx, key, strings.NewReader("stale local copy")); err != nil {
		t.Fatalf("write stale local blob: %v", err)
	}
	if _, err := destination.Put(ctx, localOnlyKey, strings.NewReader("local-only legacy blob")); err != nil {
		t.Fatalf("write local-only blob: %v", err)
	}

	var report bytes.Buffer
	summary, err := blobmigration.Restore(ctx, source, destination, &report)
	if err != nil {
		t.Fatalf("restore from S3: %v", err)
	}
	if summary.Objects != 2 || summary.Bytes != int64(len("updated from S3")+len("uploaded after cutover")) {
		t.Fatalf("summary = %#v, want two restored objects and exact bytes", summary)
	}
	for key, want := range map[string]string{
		key:          "updated from S3",
		newKey:       "uploaded after cutover",
		localOnlyKey: "local-only legacy blob",
	} {
		got, err := destination.ReadAt(ctx, key, 0, int64(len(want)+1))
		if err != nil {
			t.Fatalf("read restored blob %q: %v", key, err)
		}
		if string(got) != want {
			t.Errorf("blob %q = %q, want %q", key, got, want)
		}
	}
	if summary.SourceManifestSHA256 == "" || summary.SourceManifestSHA256 != summary.DestinationManifestSHA256 {
		t.Fatalf("manifest checksums = %#v", summary)
	}
	lines := strings.Split(strings.TrimSpace(report.String()), "\n")
	if len(lines) != summary.Objects+1 {
		t.Fatalf("report has %d lines, want %d object records and summary", len(lines), summary.Objects+1)
	}
	var gotSummary summaryReport
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &gotSummary); err != nil {
		t.Fatalf("decode restore summary: %v", err)
	}
	if gotSummary.Type != "summary" || gotSummary.Summary != summary {
		t.Fatalf("reported summary = %#v, want %#v", gotSummary, summary)
	}
}

func TestRestoreRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()

	key := blob.Key(21)
	source := &memoryStore{objects: map[string][]byte{key: []byte("source")}}
	destination := &memoryStore{objects: make(map[string][]byte), corruptPut: true}
	var report bytes.Buffer
	if _, err := blobmigration.Restore(context.Background(), source, destination, &report); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("restore error = %v, want checksum mismatch", err)
	}
	if strings.Contains(report.String(), `"type":"summary"`) {
		t.Fatalf("reported success after checksum mismatch: %q", report.String())
	}
}

type memoryStore struct {
	objects    map[string][]byte
	corruptPut bool
}

func (m *memoryStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	if m.corruptPut && len(body) > 0 {
		body[0] ^= 0xff
	}
	m.objects[key] = body
	return int64(len(body)), nil
}

func (m *memoryStore) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, ok := m.objects[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	if offset < 0 || limit < 0 {
		return nil, errors.New("invalid range")
	}
	if offset >= int64(len(body)) {
		return []byte{}, nil
	}
	end := offset + limit
	end = min(end, int64(len(body)))
	return append([]byte(nil), body[offset:end]...), nil
}

func (m *memoryStore) Remove(_ context.Context, key string) error {
	delete(m.objects, key)
	return nil
}

func (m *memoryStore) WalkPrefix(ctx context.Context, prefix string, fn func(blob.Entry) error) error {
	return m.walk(ctx, prefix, fn)
}

func (m *memoryStore) Walk(ctx context.Context, fn func(blob.Entry) error) error {
	return m.walk(ctx, "", fn)
}

func (m *memoryStore) walk(ctx context.Context, prefix string, fn func(blob.Entry) error) error {
	keys := make([]string, 0, len(m.objects))
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(blob.Entry{Key: key, Regular: true, Size: int64(len(m.objects[key]))}); err != nil {
			return err
		}
	}
	return nil
}
