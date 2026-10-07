package blobmode_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobmode"
)

const localBlobDir = "/var/lib/telegramd-blobs"

const (
	testMaxRecordBytes    = 4096
	testMaxJournalEntries = 1000
)

func TestValidateAcceptsCurrentBackendRecords(t *testing.T) {
	if !requireRootFixture(t, "TestValidateAcceptsCurrentBackendRecords") {
		return
	}
	tests := []struct {
		name     string
		outcomes []string
		config   blobmode.EffectiveConfig
	}{
		{
			name:     "initial local",
			outcomes: []string{"initial-local"},
			config:   blobmode.EffectiveConfig{BlobDir: localBlobDir},
		},
		{
			name:     "accepted S3",
			outcomes: []string{"initial-local", "s3-accepted"},
			config:   blobmode.EffectiveConfig{BlobS3: testS3Config()},
		},
		{
			name:     "recovered local restart with old publication time",
			outcomes: []string{"initial-local", "s3-accepted", "recovered-local"},
			config:   blobmode.EffectiveConfig{BlobDir: localBlobDir},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := testState(t, tt.outcomes...)
			if err := blobmode.Validate(state.path, tt.config); err != nil {
				t.Fatalf("validate current record: %v", err)
			}
		})
	}
}

func TestValidateAbsentGuardKeepsLegacyBehavior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob-mode")
	if err := blobmode.Validate(path, blobmode.EffectiveConfig{BlobS3: testS3Config()}); err != nil {
		t.Fatalf("absent guard: %v", err)
	}
}

func TestValidateIgnoresTemporaryJournalFiles(t *testing.T) {
	if !requireRootFixture(t, "TestValidateIgnoresTemporaryJournalFiles") {
		return
	}
	state := testState(t, "initial-local")
	path := filepath.Join(state.path, "journal", ".tmp-transition")
	if err := os.WriteFile(path, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := blobmode.Validate(state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}); err != nil {
		t.Fatalf("validate with ignored temporary file: %v", err)
	}
}

func TestValidateRejectsInvalidTreesWithoutChangingThem(t *testing.T) {
	if !requireRootFixture(t, "TestValidateRejectsInvalidTreesWithoutChangingThem") {
		return
	}
	tests := []struct {
		name  string
		setup func(*testing.T) (string, blobmode.EffectiveConfig)
	}{
		{
			name: "missing mode record",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				if err := os.Remove(filepath.Join(state.path, "mode.json")); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "symlinked mode file",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				mode := filepath.Join(state.path, "mode.json")
				if err := os.Remove(mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(state.path, "journal", "0000000001.json"), mode); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "symlinked journal directory",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				journal := filepath.Join(state.path, "journal")
				journalBackup := filepath.Join(state.path, "journal-real")
				if err := os.Rename(journal, journalBackup); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(journalBackup, journal); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "symlinked journal entry",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				entry := filepath.Join(state.path, "journal", "0000000001.json")
				if err := os.Remove(entry); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(state.path, "mode.json"), entry); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "symlinked guard directory",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				link := filepath.Join(t.TempDir(), "blob-mode")
				if err := os.Symlink(state.path, link); err != nil {
					t.Fatal(err)
				}
				return link, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "writable mode file",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				// #nosec G302 -- This fixture verifies rejection of writable records.
				if err := os.Chmod(filepath.Join(state.path, "mode.json"), 0o666); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "writable journal entry",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				// #nosec G302 -- This fixture verifies rejection of writable journal records.
				if err := os.Chmod(filepath.Join(state.path, "journal", "0000000001.json"), 0o666); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "writable journal directory",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				// #nosec G302 -- This fixture verifies rejection of writable directories.
				if err := os.Chmod(filepath.Join(state.path, "journal"), 0o777); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "4097-byte mode file",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				if err := os.WriteFile(filepath.Join(state.path, "mode.json"), make([]byte, testMaxRecordBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "4097-byte journal entry",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				if err := os.WriteFile(filepath.Join(state.path, "journal", "0000000001.json"), make([]byte, testMaxRecordBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "UTF-8 BOM",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				entry := filepath.Join(state.path, "journal", "0000000001.json")
				withBOM := append([]byte{0xef, 0xbb, 0xbf}, state.mode...)
				writeBoth(t, state.path, entry, withBOM)
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "invalid UTF-8",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				invalid := append(append([]byte(nil), state.mode...), 0xff)
				writeBoth(t, state.path, filepath.Join(state.path, "journal", "0000000001.json"), invalid)
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "duplicate nested key",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				duplicate := strings.Replace(string(state.mode), `"kind":"local"`, `"kind":"local","kind":"local"`, 1)
				writeBoth(t, state.path, filepath.Join(state.path, "journal", "0000000001.json"), []byte(duplicate))
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "unknown field",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				unknown := strings.TrimSuffix(string(state.mode), "}") + `,"unexpected":true}`
				writeBoth(t, state.path, filepath.Join(state.path, "journal", "0000000001.json"), []byte(unknown))
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "journal gap",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				chain := testChain(t, "initial-local", "s3-accepted", "recovered-local")
				delete(chain.records, 2)
				return writeTestState(t, chain.records, chain.records[3]), blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "recovered local follows initial local without S3 acceptance",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				chain := testChain(t, "initial-local", "recovered-local")
				return writeTestState(t, chain.records, chain.records[2]), blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "stale mode record",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				chain := testChain(t, "initial-local", "s3-accepted", "recovered-local")
				return writeTestState(t, chain.records, chain.records[1]), blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "reused transition id",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				chain := testChain(t, "initial-local", "s3-accepted")
				reused := mutateRecord(t, chain.records[2], func(record map[string]any) {
					record["transition_id"] = recordID(1)
				})
				chain.records[2] = reused
				return writeTestState(t, chain.records, reused), blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "reused report digest",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				chain := testChain(t, "initial-local", "s3-accepted")
				reused := mutateRecord(t, chain.records[2], func(record map[string]any) {
					objectField(t, record, "evidence")["report_sha256"] = digest(1)
				})
				chain.records[2] = reused
				return writeTestState(t, chain.records, reused), blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "wrong local backend",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				return state.path, blobmode.EffectiveConfig{BlobDir: "/elsewhere"}
			},
		},
		{
			name: "wrong S3 endpoint",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				cfg := testS3Config()
				cfg.Endpoint = "http://other-rustfs:9000"
				return state.path, blobmode.EffectiveConfig{BlobS3: cfg}
			},
		},
		{
			name: "wrong S3 bucket",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				cfg := testS3Config()
				cfg.Bucket = "other-bucket"
				return state.path, blobmode.EffectiveConfig{BlobS3: cfg}
			},
		},
		{
			name: "wrong S3 prefix",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				cfg := testS3Config()
				cfg.Prefix = "other-prefix"
				return state.path, blobmode.EffectiveConfig{BlobS3: cfg}
			},
		},
		{
			name: "unnormalized S3 record prefix",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				changed := mutateRecord(t, state.mode, func(record map[string]any) {
					objectField(t, record, "backend")["prefix"] = "telegramd"
				})
				return writeTestState(t, withChangedHead(state, changed), changed), blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "recovered local does not match default S3 backend",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted", "recovered-local")
				return state.path, blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "unsupported fresh S3 outcome",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local")
				changed := mutateRecord(t, state.mode, func(record map[string]any) {
					record["outcome"] = "fresh-s3"
				})
				return writeTestState(t, withChangedHead(state, changed), changed), blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "S3 record does not match local backend",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				return state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "missing mandatory evidence",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				changed := mutateRecord(t, state.mode, func(record map[string]any) {
					delete(objectField(t, record, "evidence"), "source_manifest_sha256")
				})
				return writeTestState(t, withChangedHead(state, changed), changed), blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "missing mandatory restore evidence",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted", "recovered-local")
				changed := mutateRecord(t, state.mode, func(record map[string]any) {
					delete(objectField(t, record, "evidence"), "restored_manifest_sha256")
				})
				return writeTestState(t, withChangedHead(state, changed), changed), blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
		{
			name: "S3 null rustfsdata volume",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				changed := mutateRecord(t, state.mode, func(record map[string]any) {
					objectField(t, record, "volumes")["rustfsdata"] = nil
				})
				return writeTestState(t, withChangedHead(state, changed), changed), blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "copy pass count is not two",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted")
				changed := mutateRecord(t, state.mode, func(record map[string]any) {
					objectField(t, record, "evidence")["copy_passes"] = 1
				})
				return writeTestState(t, withChangedHead(state, changed), changed), blobmode.EffectiveConfig{BlobS3: testS3Config()}
			},
		},
		{
			name: "restore pass count is not two",
			setup: func(t *testing.T) (string, blobmode.EffectiveConfig) {
				t.Helper()
				state := testState(t, "initial-local", "s3-accepted", "recovered-local")
				changed := mutateRecord(t, state.mode, func(record map[string]any) {
					objectField(t, record, "evidence")["restore_passes"] = 1
				})
				return writeTestState(t, withChangedHead(state, changed), changed), blobmode.EffectiveConfig{BlobDir: localBlobDir}
			},
		},
	}
	// #nosec G101 -- Values are fixed validator error strings, not credentials.
	wantErrors := map[string]string{
		"missing mode record":         "reason=invalid field=mode.json",
		"symlinked mode file":         "reason=invalid field=mode.json",
		"symlinked journal directory": "reason=invalid field=journal",
		"symlinked journal entry":     "reason=invalid field=journal",
		"symlinked guard directory":   "reason=invalid field=blob-mode",
		"writable mode file":          "reason=permissions field=mode.json",
		"writable journal entry":      "reason=permissions field=journal",
		"writable journal directory":  "reason=permissions field=journal",
		"4097-byte mode file":         "reason=oversize field=mode.json",
		"4097-byte journal entry":     "reason=oversize field=journal",
		"UTF-8 BOM":                   "reason=encoding field=record",
		"invalid UTF-8":               "reason=encoding field=record",
		"duplicate nested key":        "reason=schema field=record",
		"unknown field":               "reason=schema field=record",
		"journal gap":                 "reason=sequence field=journal",
		"recovered local follows initial local without S3 acceptance": "reason=supersession field=outcome",
		"stale mode record":                                 "reason=stale field=mode.json",
		"reused transition id":                              "reason=reused field=transition_id",
		"reused report digest":                              "reason=reused field=evidence.report_sha256",
		"wrong local backend":                               "reason=backend field=backend",
		"wrong S3 endpoint":                                 "reason=backend field=backend",
		"wrong S3 bucket":                                   "reason=backend field=backend",
		"wrong S3 prefix":                                   "reason=backend field=backend",
		"unnormalized S3 record prefix":                     "reason=schema field=backend.prefix",
		"recovered local does not match default S3 backend": "reason=backend field=backend",
		"unsupported fresh S3 outcome":                      "reason=schema field=evidence",
		"S3 record does not match local backend":            "reason=backend field=backend",
		"missing mandatory evidence":                        "reason=schema field=evidence",
		"missing mandatory restore evidence":                "reason=schema field=evidence",
		"S3 null rustfsdata volume":                         "reason=schema field=volumes.rustfsdata",
		"copy pass count is not two":                        "reason=schema field=evidence",
		"restore pass count is not two":                     "reason=schema field=evidence",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, cfg := tt.setup(t)
			before := snapshotTree(t, path)
			err := blobmode.Validate(path, cfg)
			if err == nil {
				t.Fatal("validate succeeded, want rejection")
			}
			if !strings.Contains(err.Error(), "reason=") || !strings.Contains(err.Error(), "field=") {
				t.Fatalf("validation error is not fixed-code plus field: %v", err)
			}
			want, ok := wantErrors[tt.name]
			if !ok {
				t.Fatalf("missing expected fixed error for fixture %q", tt.name)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("validation error = %v, want %q", err, want)
			}
			if strings.Contains(err.Error(), localBlobDir) || strings.Contains(err.Error(), "project_tgblobs") {
				t.Fatalf("validation error exposed record values: %v", err)
			}
			after := snapshotTree(t, path)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("validator changed the state tree\nbefore: %#v\nafter:  %#v", before, after)
			}
		})
	}
}

func TestValidateRejectsMoreThan1000JournalEntries(t *testing.T) {
	if !requireRootFixture(t, "TestValidateRejectsMoreThan1000JournalEntries") {
		return
	}
	state := testState(t, "initial-local")
	journal := filepath.Join(state.path, "journal")
	for generation := 2; generation <= testMaxJournalEntries+1; generation++ {
		path := filepath.Join(journal, fmt.Sprintf("%010d.json", generation))
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotTree(t, state.path)
	err := blobmode.Validate(state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir})
	if err == nil {
		t.Fatal("validate succeeded with more than 1000 journal entries")
	}
	if !strings.Contains(err.Error(), "reason=oversize field=journal") {
		t.Fatalf("validation error = %v, want bounded-journal rejection", err)
	}
	if !reflect.DeepEqual(snapshotTree(t, state.path), before) {
		t.Fatal("validator changed overfull journal")
	}
}

func TestValidateRequiresRootOwnedState(t *testing.T) {
	if !requireRootFixture(t, "TestValidateRequiresRootOwnedState") {
		return
	}
	state := testState(t, "initial-local")
	if err := os.Chown(filepath.Join(state.path, "mode.json"), 65534, -1); err != nil {
		t.Fatalf("make wrong-owner fixture: %v", err)
	}
	before := snapshotTree(t, state.path)
	err := blobmode.Validate(state.path, blobmode.EffectiveConfig{BlobDir: localBlobDir})
	if err == nil {
		t.Fatal("validate succeeded with non-root-owned fixture")
	}
	if !strings.Contains(err.Error(), "reason=ownership") {
		t.Fatalf("validation error = %v, want root-ownership rejection", err)
	}
	if !reflect.DeepEqual(snapshotTree(t, state.path), before) {
		t.Fatal("validator changed wrong-owner state")
	}
}

func requireRootFixture(t *testing.T, testName string) bool {
	t.Helper()
	if os.Geteuid() == 0 {
		return true
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		t.Skip("root-owned blob-mode fixture requires the CI root test environment")
		return false
	}
	// #nosec G204 G702 -- Run only this test binary and fixed test name under sudo for root-owned fixtures.
	command := exec.CommandContext(t.Context(), sudo, "-n", os.Args[0], "-test.run=^"+testName+"$")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("root fixture failed: %v\n%s", err, output)
	}
	return false
}

type testStateData struct {
	path    string
	mode    []byte
	records map[int][]byte
}

func testState(t *testing.T, outcomes ...string) testStateData {
	t.Helper()
	chain := testChain(t, outcomes...)
	path := writeTestState(t, chain.records, chain.records[len(chain.records)])
	return testStateData{path: path, mode: chain.records[len(chain.records)], records: chain.records}
}

func testChain(t *testing.T, outcomes ...string) struct{ records map[int][]byte } {
	t.Helper()
	records := make(map[int][]byte, len(outcomes))
	previous := ""
	for index, outcome := range outcomes {
		generation := index + 1
		records[generation] = makeRecord(t, generation, outcome, previous, nil)
		previous = recordID(generation)
	}
	return struct{ records map[int][]byte }{records: records}
}

func makeRecord(t *testing.T, generation int, outcome, supersedes string, change func(map[string]any)) []byte {
	t.Helper()
	backend := map[string]any{"kind": "local", "dir": localBlobDir}
	volumes := map[string]any{"tgblobs": "project_tgblobs", "rustfsdata": nil}
	evidence := map[string]any{"report_sha256": digest(generation)}
	switch outcome {
	case "initial-local":
	case "s3-accepted":
		backend = map[string]any{"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "telegram", "prefix": "telegramd/"}
		volumes["rustfsdata"] = "project_rustfsdata"
		evidence["source_manifest_sha256"] = digest(generation + 10)
		evidence["destination_manifest_sha256"] = digest(generation + 20)
		evidence["object_count"] = 12
		evidence["byte_total"] = 2048
		evidence["copy_passes"] = 2
	case "recovered-local":
		volumes["rustfsdata"] = "project_rustfsdata"
		evidence["s3_census_manifest_sha256"] = digest(generation + 10)
		evidence["restored_manifest_sha256"] = digest(generation + 20)
		evidence["object_count"] = 12
		evidence["byte_total"] = 2048
		evidence["restore_passes"] = 2
		evidence["retained_cutover_key_count"] = 1
	default:
		t.Fatalf("unknown test outcome %q", outcome)
	}
	var supersedesValue any
	if generation != 1 {
		supersedesValue = supersedes
	}
	record := map[string]any{
		"schema":        "teagram.blob-mode/v1",
		"generation":    generation,
		"transition_id": recordID(generation),
		"supersedes":    supersedesValue,
		"outcome":       outcome,
		"backend":       backend,
		"volumes":       volumes,
		"evidence":      evidence,
		"published_at":  "2020-01-01T00:00:00Z",
	}
	if change != nil {
		change(record)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return data
}

func writeTestState(t *testing.T, records map[int][]byte, mode []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob-mode")
	if err := os.MkdirAll(filepath.Join(path, "journal"), 0o700); err != nil {
		t.Fatal(err)
	}
	for generation, record := range records {
		if err := os.WriteFile(filepath.Join(path, "journal", fmt.Sprintf("%010d.json", generation)), record, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if mode != nil {
		if err := os.WriteFile(filepath.Join(path, "mode.json"), mode, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func writeBoth(t *testing.T, statePath, journalPath string, data []byte) {
	t.Helper()
	if err := os.WriteFile(journalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "mode.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func withChangedHead(state testStateData, changed []byte) map[int][]byte {
	records := maps.Clone(state.records)
	records[len(records)] = changed
	return records
}

func objectField(t *testing.T, record map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := record[key].(map[string]any)
	if !ok {
		t.Fatalf("record field %q is not an object", key)
	}
	return value
}

func mutateRecord(t *testing.T, data []byte, change func(map[string]any)) []byte {
	t.Helper()
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode record for fixture: %v", err)
	}
	change(record)
	changed, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal changed fixture: %v", err)
	}
	return changed
}

func recordID(generation int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", generation)
}

func digest(value int) string {
	return strings.Repeat(strconv.Itoa(value%10), 64)
}

func testS3Config() *blob.S3Config {
	return &blob.S3Config{Endpoint: "http://rustfs:9000", Bucket: "telegram", Prefix: "telegramd"}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	var walk func(string, string)
	walk = func(path, relative string) {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		value := info.Mode().String()
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			snapshot[relative] = "symlink:" + target
			return
		}
		if info.IsDir() {
			snapshot[relative] = value
			entries, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				walk(filepath.Join(path, entry.Name()), filepath.Join(relative, entry.Name()))
			}
			return
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			value += ":" + string(data)
		}
		snapshot[relative] = value
	}
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return snapshot
		}
		t.Fatal(err)
	}
	walk(root, ".")
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(root)
		if err != nil {
			t.Fatal(err)
		}
		targetPath := target
		if !filepath.IsAbs(targetPath) {
			targetPath = filepath.Join(filepath.Dir(root), targetPath)
		}
		walk(targetPath, "target")
	}
	return snapshot
}
