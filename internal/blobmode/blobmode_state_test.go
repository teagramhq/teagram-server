//nolint:testpackage // The expected-UID seam keeps filesystem authority tests unprivileged.
package blobmode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
)

func TestReadReturnsValidatedS3Authority(t *testing.T) {
	t.Parallel()
	cfg := EffectiveConfig{BlobS3: &blob.S3Config{
		Endpoint: "http://rustfs:9000",
		Bucket:   "teagram",
		Prefix:   "teagramd/",
	}}
	path := writeS3AcceptedAuthority(t)

	got, err := readAt(path, cfg, testUID(t))
	if err != nil {
		t.Fatalf("read authority: %v", err)
	}
	if !got.Present || got.Outcome != OutcomeS3Accepted {
		t.Fatalf("authority = %+v, want present %q", got, OutcomeS3Accepted)
	}
}

func TestReadReturnsAbsentAuthorityForMissingDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing")
	got, err := readAt(path, EffectiveConfig{BlobS3: &blob.S3Config{}}, testUID(t))
	if err != nil {
		t.Fatalf("read absent authority: %v", err)
	}
	if got.Present || got.Outcome != "" {
		t.Fatalf("authority = %+v, want absent", got)
	}
}

func testUID(t *testing.T) uint32 {
	t.Helper()
	uid := uint64(os.Getuid()) //nolint:gosec // POSIX uid is nonnegative; the range is checked below.
	if uid > uint64(^uint32(0)) {
		t.Fatalf("uid %d does not fit blob-mode owner field", uid)
	}
	return uint32(uid) //nolint:gosec // The range is checked immediately above.
}

func writeS3AcceptedAuthority(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob-mode")
	journal := filepath.Join(path, "journal")
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	initial := authorityRecord(t, 1, "initial-local", "")
	accepted := authorityRecord(t, 2, OutcomeS3Accepted, "00000000-0000-4000-8000-000000000001")
	for generation, record := range [][]byte{initial, accepted} {
		name := fmt.Sprintf("%010d.json", generation+1)
		if err := os.WriteFile(filepath.Join(journal, name), record, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "mode.json"), accepted, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func authorityRecord(t *testing.T, generation int, outcome, supersedes string) []byte {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", generation)
	report := fmt.Sprintf("%064x", generation)
	var supersedesValue any
	if supersedes != "" {
		supersedesValue = supersedes
	}
	record := map[string]any{
		"schema":        "teagram.blob-mode/v1",
		"generation":    generation,
		"transition_id": id,
		"supersedes":    supersedesValue,
		"outcome":       outcome,
		"backend":       map[string]any{"kind": "local", "dir": "/var/lib/teagramd-blobs"},
		"volumes":       map[string]any{"tgblobs": "test_tgblobs", "rustfsdata": nil},
		"evidence":      map[string]any{"report_sha256": report},
		"published_at":  "2026-10-24T00:00:00Z",
	}
	if outcome == OutcomeS3Accepted {
		record["backend"] = map[string]any{"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "teagram", "prefix": "teagramd/"}
		record["volumes"] = map[string]any{"tgblobs": "test_tgblobs", "rustfsdata": "test_rustfsdata"}
		record["evidence"] = map[string]any{
			"report_sha256": report, "source_manifest_sha256": fmt.Sprintf("%064x", generation+10),
			"destination_manifest_sha256": fmt.Sprintf("%064x", generation+20),
			"object_count":                0, "byte_total": 0, "copy_passes": 1,
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
