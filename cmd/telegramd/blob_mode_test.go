package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunRejectsRecoveredLocalBeforeS3OrListeners(t *testing.T) {
	if !runRootOnlyFixture(t) {
		return
	}

	var requests atomic.Int64
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		if _, err := io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`); err != nil {
			t.Errorf("write fake S3 response: %v", err)
		}
	}))
	t.Cleanup(s3.Close)

	modeDir := writeRecoveredLocalFixture(t)
	before := snapshotBlobMode(t, modeDir)
	var listenConfig net.ListenConfig
	reserved, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen address: %v", err)
	}
	listenAddr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatalf("release listen address: %v", err)
	}

	setIdentityRunConfig(t, filepath.Join(t.TempDir(), "missing-key.pem"))
	t.Setenv("TG_LISTEN_ADDR", listenAddr)
	t.Setenv("TG_BLOB_DIR", "/var/lib/telegramd-blobs")
	for _, name := range []string{
		"TG_BLOB_S3_ENDPOINT", "TG_BLOB_S3_BUCKET", "TG_BLOB_S3_PREFIX", "TG_BLOB_S3_REGION",
		"TG_BLOB_S3_ACCESS_KEY_ID", "TG_BLOB_S3_SECRET_ACCESS_KEY", "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE",
		"TG_BLOB_S3_CA_PATH", "TG_BLOB_S3_ALLOW_INSECURE_HTTP",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("TG_BLOB_S3_ENDPOINT", s3.URL)
	t.Setenv("TG_BLOB_S3_BUCKET", "telegram")
	t.Setenv("TG_BLOB_S3_PREFIX", "telegramd")
	t.Setenv("TG_BLOB_S3_ACCESS_KEY_ID", "test-access")
	t.Setenv("TG_BLOB_S3_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("TG_BLOB_S3_ALLOW_INSECURE_HTTP", "true")

	err = runAtBlobModePath(slog.New(slog.DiscardHandler), modeDir)
	if err == nil || err.Error() != "blob-mode validation failed: reason=backend field=backend" {
		t.Fatalf("run error = %v, want a fixed backend-mismatch error", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("fake S3 requests = %d, want 0", got)
	}
	if after := snapshotBlobMode(t, modeDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("startup changed blob-mode state\nbefore: %#v\nafter:  %#v", before, after)
	}
	listener, err := listenConfig.Listen(t.Context(), "tcp", listenAddr)
	if err != nil {
		t.Fatalf("run opened serving listener at %s: %v", listenAddr, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close verification listener: %v", err)
	}
}

func runRootOnlyFixture(t *testing.T) bool {
	t.Helper()
	if os.Geteuid() == 0 {
		return true
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		t.Skip("root-owned blob-mode fixture requires the CI root test environment")
		return false
	}
	// #nosec G204 G702 -- Run only this test binary under sudo for a root-owned fixture.
	command := exec.CommandContext(t.Context(), sudo, "-n", os.Args[0], "-test.run=^TestRunRejectsRecoveredLocalBeforeS3OrListeners$")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("root fixture failed: %v\n%s", err, output)
	}
	return false
}

func writeRecoveredLocalFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "blob-mode")
	journal := filepath.Join(root, "journal")
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := ""
	var head []byte
	for generation, outcome := range []string{"initial-local", "s3-accepted", "recovered-local"} {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012x", generation+1)
		var backend map[string]any
		volumes := map[string]any{"tgblobs": "project_tgblobs", "rustfsdata": nil}
		evidence := map[string]any{"report_sha256": strings.Repeat(strconv.Itoa(generation+1), 64)}
		switch outcome {
		case "initial-local":
			backend = map[string]any{"kind": "local", "dir": "/var/lib/telegramd-blobs"}
		case "s3-accepted":
			backend = map[string]any{"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "telegram", "prefix": "telegramd/"}
			volumes["rustfsdata"] = "project_rustfsdata"
			evidence["source_manifest_sha256"] = strings.Repeat("a", 64)
			evidence["destination_manifest_sha256"] = strings.Repeat("b", 64)
			evidence["object_count"] = 1
			evidence["byte_total"] = 10
			evidence["copy_passes"] = 2
		case "recovered-local":
			backend = map[string]any{"kind": "local", "dir": "/var/lib/telegramd-blobs"}
			volumes["rustfsdata"] = "project_rustfsdata"
			evidence["s3_census_manifest_sha256"] = strings.Repeat("c", 64)
			evidence["restored_manifest_sha256"] = strings.Repeat("d", 64)
			evidence["object_count"] = 1
			evidence["byte_total"] = 10
			evidence["restore_passes"] = 2
			evidence["retained_cutover_key_count"] = 0
		}
		var supersedes any
		if generation > 0 {
			supersedes = previous
		}
		record := map[string]any{
			"schema": "teagram.blob-mode/v1", "generation": generation + 1, "transition_id": id,
			"supersedes": supersedes, "outcome": outcome, "backend": backend, "volumes": volumes,
			"evidence": evidence, "published_at": "2020-01-01T00:00:00Z",
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		if err := os.WriteFile(filepath.Join(journal, fmt.Sprintf("%010d.json", generation+1)), data, 0o600); err != nil {
			t.Fatal(err)
		}
		previous = id
		head = data
	}
	if err := os.WriteFile(filepath.Join(root, "mode.json"), head, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func snapshotBlobMode(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	var walk func(string, string)
	walk = func(path, relative string) {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			result[relative] = "symlink:" + target
			return
		}
		if info.IsDir() {
			result[relative] = "directory"
			entries, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				walk(filepath.Join(path, entry.Name()), filepath.Join(relative, entry.Name()))
			}
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[relative] = string(data)
	}
	walk(root, ".")
	return result
}
