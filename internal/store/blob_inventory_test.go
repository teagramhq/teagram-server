package store_test

import (
	"context"
	_ "embed"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
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
	type sourceRow struct {
		kind         string
		fileID       int64
		blobKey      string
		logicalBytes int64
	}
	var sourceRows []sourceRow
	var canonical []store.BlobInventoryReference
	for rows.Next() {
		var row sourceRow
		if err := rows.Scan(&row.kind, &row.fileID, &row.blobKey, &row.logicalBytes); err != nil {
			rows.Close()
			t.Fatalf("scan canonical inventory row: %v", err)
		}
		sourceRows = append(sourceRows, row)
		switch row.kind {
		case "file":
			if row.fileID <= 0 || row.blobKey != "" {
				rows.Close()
				t.Fatalf("file inventory row = %+v, want positive file id and empty blob key", row)
			}
			canonical = append(canonical, store.BlobInventoryReference{
				Key:          blob.Key(row.fileID),
				Kind:         row.kind,
				ID:           strconv.FormatInt(row.fileID, 10),
				LogicalBytes: row.logicalBytes,
			})
		case "part":
			if row.fileID != 0 || row.blobKey == "" {
				rows.Close()
				t.Fatalf("part inventory row = %+v, want zero file id and non-empty blob key", row)
			}
			canonical = append(canonical, store.BlobInventoryReference{
				Key:          row.blobKey,
				Kind:         row.kind,
				ID:           row.blobKey,
				LogicalBytes: row.logicalBytes,
			})
		default:
			rows.Close()
			t.Fatalf("canonical inventory row has unexpected kind %q", row.kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read canonical inventory rows: %v", err)
	}
	rows.Close()

	got, err := s.BlobInventoryReferences(ctx)
	if err != nil {
		t.Fatalf("list generated inventory references: %v", err)
	}
	wantSourceRows := []sourceRow{
		{kind: "file", fileID: stored.ID, blobKey: "", logicalBytes: 6},
		{kind: "part", fileID: 0, blobKey: partKey, logicalBytes: 4},
	}
	want := []store.BlobInventoryReference{
		{Key: blob.Key(stored.ID), Kind: "file", ID: strconv.FormatInt(stored.ID, 10), LogicalBytes: 6},
		{Key: partKey, Kind: "part", ID: partKey, LogicalBytes: 4},
	}
	if !reflect.DeepEqual(canonical, got) {
		t.Fatalf("canonical SQL references = %+v, generated query references = %+v", canonical, got)
	}
	if !reflect.DeepEqual(sourceRows, wantSourceRows) {
		t.Fatalf("canonical SQL rows = %+v, want parser contract rows %+v (pending file %d excluded)", sourceRows, wantSourceRows, pending.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generated query references = %+v, want %+v", got, want)
	}
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
