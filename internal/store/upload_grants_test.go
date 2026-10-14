package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestConcurrentUploadGrantLimitCleansLosingAssembly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn := pgtest.DSN(t)
	blobs := testBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close first store: %v", err)
		}
	})
	other, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
	})
	uploader := mustUser(t, s, "+15559108001")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect grant fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close grant fixture: %v", err)
		}
	})
	for range store.MaxUploadGrantsPerAccount - 1 {
		file := storedFileWithBytes(t, s, uploader.ID)
		if _, err := conn.Exec(ctx,
			`INSERT INTO file_upload_grants (file_id, owner_id) VALUES ($1, $2)`, file.ID, uploader.ID); err != nil {
			t.Fatalf("seed upload grant: %v", err)
		}
	}
	rowsBefore, bytesBefore, err := uploadGrantAccountState(ctx, conn, uploader.ID)
	if err != nil {
		t.Fatalf("read upload grant state before contenders: %v", err)
	}

	started := make(chan int64, 2)
	release := make(chan struct{})
	type result struct {
		attemptedID int64
		file        store.File
		err         error
	}
	results := make(chan result, 2)
	for attempt := range 2 {
		go func(attempt int) {
			payload := []byte("concurrent upload grant candidate")
			var attemptedID int64
			ownerStore := s
			if attempt == 1 {
				ownerStore = other
			}
			file, err := ownerStore.AllocateAndCompleteFileWithUploadGrant(
				ctx, uploader.ID, int64(len(payload)), "text/plain", fmt.Sprintf("contender-%d.txt", attempt), 1<<30, nil, 6*time.Hour,
				func(file store.File) error {
					attemptedID = file.ID
					started <- file.ID
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					_, err := store.BlobsOf(ownerStore).Put(ctx, blob.Key(file.ID), bytes.NewReader(payload))
					return err
				},
			)
			results <- result{attemptedID: attemptedID, file: file, err: err}
		}(attempt)
	}
	ids := make([]int64, 0, 2)
	for range 2 {
		select {
		case id := <-started:
			ids = append(ids, id)
		case <-ctx.Done():
			t.Fatalf("both contenders did not reach blob assembly: %v", ctx.Err())
		}
	}
	if ids[0] == ids[1] {
		t.Fatalf("contenders shared file id %d", ids[0])
	}
	close(release)

	var winnerID, loserID int64
	var limitErrors int
	for range 2 {
		select {
		case got := <-results:
			if got.err == nil {
				if winnerID != 0 {
					t.Fatal("both concurrent uploadMedia contenders succeeded")
				}
				winnerID = got.file.ID
				continue
			}
			if !errors.Is(got.err, store.ErrUploadGrantLimit) {
				t.Fatalf("losing contender error = %v, want ErrUploadGrantLimit", got.err)
			}
			limitErrors++
			loserID = got.attemptedID
		case <-ctx.Done():
			t.Fatalf("contender did not finish: %v", ctx.Err())
		}
	}
	if winnerID == 0 || loserID == 0 || winnerID == loserID || limitErrors != 1 {
		t.Fatalf("winner/loser/limit errors = %d/%d/%d, want distinct IDs and one loser", winnerID, loserID, limitErrors)
	}
	if !rowPresent(t, s, winnerID) || !blobPresent(t, s, winnerID) {
		t.Fatalf("winning file %d lost its row or blob", winnerID)
	}
	if rowPresent(t, s, loserID) || blobPresent(t, s, loserID) {
		t.Fatalf("losing file %d retained its row or blob", loserID)
	}
	rowsAfter, bytesAfter, err := uploadGrantAccountState(ctx, conn, uploader.ID)
	if err != nil {
		t.Fatalf("read upload grant state after contenders: %v", err)
	}
	if rowsAfter != rowsBefore+1 || bytesAfter != bytesBefore+int64(len("concurrent upload grant candidate")) {
		t.Fatalf("rows/bytes after contenders = %d/%d, before=%d/%d; want only the winning file charged", rowsAfter, bytesAfter, rowsBefore, bytesBefore)
	}
	var grants int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM file_upload_grants WHERE owner_id = $1`, uploader.ID).Scan(&grants); err != nil {
		t.Fatalf("count grants after contenders: %v", err)
	}
	if grants != store.MaxUploadGrantsPerAccount {
		t.Fatalf("upload grants = %d, want cap %d", grants, store.MaxUploadGrantsPerAccount)
	}
}

func uploadGrantAccountState(ctx context.Context, conn *pgx.Conn, ownerID int64) (rows, bytes int64, err error) {
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM files WHERE uploader_id = $1`, ownerID).Scan(&rows); err != nil {
		return 0, 0, err
	}
	err = conn.QueryRow(ctx, `
		SELECT coalesce(sum(f.size + coalesce(octet_length(d.m_bytes), 0) + coalesce(octet_length(d.stripped), 0)), 0)::bigint
		FROM files f
		LEFT JOIN photo_derivatives d ON d.file_id = f.id
		WHERE f.uploader_id = $1
	`, ownerID).Scan(&bytes)
	return rows, bytes, err
}
