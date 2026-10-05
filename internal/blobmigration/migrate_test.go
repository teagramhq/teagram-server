package blobmigration_test

import (
	"bytes"
	"context"
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
