package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
	storedb "github.com/teagramhq/teagram-server/internal/store/db"
)

//go:embed queries/uploads.sql
var uploadsQueries string

func TestBlobInventoryReferencesMatchesCanonicalSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect canonical query: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close canonical query connection: %v", err)
		}
	})

	u, err := s.CreateUser(ctx, "+155590002426")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	stored, err := s.AllocateFile(ctx, u.ID, 6, "text/plain", "stored.txt", 1<<20)
	if err != nil {
		t.Fatalf("allocate stored file: %v", err)
	}
	if err := s.MarkFileStored(ctx, stored.ID); err != nil {
		t.Fatalf("mark file stored: %v", err)
	}
	pending, err := s.AllocateFile(ctx, u.ID, 7, "text/plain", "pending.txt", 1<<20)
	if err != nil {
		t.Fatalf("allocate pending file: %v", err)
	}
	partKey := "parts/ab/" + strings.Repeat("0", 30)
	if err := store.InsertUploadPartWithKey(ctx, s, u.ID, 17, 0, 4, partKey); err != nil {
		t.Fatalf("insert live part: %v", err)
	}
	if err := store.InsertUploadPartWithoutKey(ctx, s, u.ID, 18, 0, 5); err != nil {
		t.Fatalf("insert part without key: %v", err)
	}

	rows, err := conn.Query(ctx, canonicalBlobInventoryReferencesSQL(t))
	if err != nil {
		t.Fatalf("execute canonical inventory SQL: %v", err)
	}
	var canonicalRows []storedb.BlobInventoryReferencesRow
	for rows.Next() {
		var row storedb.BlobInventoryReferencesRow
		if err := rows.Scan(&row.Kind, &row.FileID, &row.BlobKey, &row.LogicalBytes); err != nil {
			rows.Close()
			t.Fatalf("scan canonical inventory row: %v", err)
		}
		canonicalRows = append(canonicalRows, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read canonical inventory rows: %v", err)
	}
	rows.Close()

	generatedRows, err := storedb.New(conn).BlobInventoryReferences(ctx)
	if err != nil {
		t.Fatalf("execute generated inventory query: %v", err)
	}
	if !reflect.DeepEqual(canonicalRows, generatedRows) {
		t.Fatalf("canonical SQL rows = %+v, generated query rows = %+v", canonicalRows, generatedRows)
	}

	got, err := s.BlobInventoryReferences(ctx)
	if err != nil {
		t.Fatalf("list generated inventory references: %v", err)
	}
	wantCanonicalRows := []storedb.BlobInventoryReferencesRow{
		{Kind: "file", FileID: stored.ID, BlobKey: "", LogicalBytes: 6},
		{Kind: "part", FileID: 0, BlobKey: partKey, LogicalBytes: 4},
	}
	want := []store.BlobInventoryReference{
		{Key: blob.Key(stored.ID), Kind: "file", ID: strconv.FormatInt(stored.ID, 10), LogicalBytes: 6},
		{Key: partKey, Kind: "part", ID: partKey, LogicalBytes: 4},
	}
	if !reflect.DeepEqual(canonicalRows, wantCanonicalRows) {
		t.Fatalf("canonical SQL rows = %+v, want census rows %+v (pending file %d excluded)", canonicalRows, wantCanonicalRows, pending.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generated query references = %+v, want %+v", got, want)
	}
	parsed := parseBlobInventoryCensusReferences(t, blobInventoryReferencesTSV(canonicalRows))
	storeReferences := make([]censusReference, 0, len(got))
	for _, ref := range got {
		storeReferences = append(storeReferences, censusReference{Key: ref.Key, LogicalBytes: ref.LogicalBytes})
	}
	sort.Slice(storeReferences, func(i, j int) bool { return storeReferences[i].Key < storeReferences[j].Key })
	if !reflect.DeepEqual(parsed, storeReferences) {
		t.Fatalf("census parser references = %+v, Store references = %+v", parsed, storeReferences)
	}
}

type censusReference struct {
	Key          string `json:"key"`
	LogicalBytes int64  `json:"logical_bytes"`
}

func blobInventoryReferencesTSV(rows []storedb.BlobInventoryReferencesRow) []byte {
	var tsv []byte
	for _, row := range rows {
		tsv = append(tsv, row.Kind...)
		tsv = append(tsv, '\t')
		tsv = strconv.AppendInt(tsv, row.FileID, 10)
		tsv = append(tsv, '\t')
		tsv = append(tsv, row.BlobKey...)
		tsv = append(tsv, '\t')
		tsv = strconv.AppendInt(tsv, row.LogicalBytes, 10)
		tsv = append(tsv, '\n')
	}
	return tsv
}

func parseBlobInventoryCensusReferences(t *testing.T, tsv []byte) []censusReference {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate store test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "../.."))
	parserPath := filepath.Join(repoRoot, "deploy/teagramd/rollout-runner/attest-blob-mode-reports.py")
	const script = `import json, runpy, sys
module = runpy.run_path(sys.argv[1])
print(json.dumps(module["parse_references"](sys.stdin.buffer.read())))
`
	// #nosec G204 -- Run the repository's fixed census parser on fixture rows.
	command := exec.CommandContext(t.Context(), "python3", "-c", script, parserPath)
	command.Stdin = bytes.NewReader(tsv)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run census reference parser: %v\n%s", err, output)
	}
	var references []censusReference
	if err := json.Unmarshal(output, &references); err != nil {
		t.Fatalf("decode census parser references: %v\n%s", err, output)
	}
	return references
}

func canonicalBlobInventoryReferencesSQL(t *testing.T) string {
	t.Helper()
	lines := strings.Split(uploadsQueries, "\n")
	start := -1
	for i, line := range lines {
		if line == "-- name: BlobInventoryReferences :many" {
			start = i
			break
		}
	}
	if start == -1 {
		t.Fatal("canonical BlobInventoryReferences query is missing")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "-- name: ") {
			end = i
			break
		}
	}
	query := strings.TrimSpace(strings.Join(lines[start+1:end], "\n"))
	if !strings.HasSuffix(query, ";") {
		t.Fatal("canonical BlobInventoryReferences query has no terminating semicolon")
	}
	return query
}
