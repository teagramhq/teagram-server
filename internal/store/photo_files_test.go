package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestStoredDocumentsDefaultToDocumentMetadata(t *testing.T) {
	t.Parallel()
	s := open(t)
	uploader := mustUser(t, s, "+15559101001")
	f := storedFile(t, s, uploader.ID)

	files, err := s.FilesByIDs(context.Background(), []int64{f.ID})
	if err != nil {
		t.Fatalf("load stored file metadata: %v", err)
	}
	got, ok := files[f.ID]
	if !ok {
		t.Fatalf("stored file %d missing from file metadata", f.ID)
	}

	if got.Kind != store.FileKindDocument {
		t.Fatalf("stored file kind = %q, want document", got.Kind)
	}
	if got.Width != 0 || got.Height != 0 {
		t.Fatalf("document dimensions = %d x %d, want 0 x 0", got.Width, got.Height)
	}
}

func TestPhotoMetadataPersistsAcrossStoreRecreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := testBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // best-effort close
	uploader := mustUser(t, s, "+15559101002")

	file, err := s.AllocateAndCompletePhotoFile(ctx, uploader.ID, 10, "image/jpeg", "photo.jpg", int64(1<<30), func(file store.File) (store.PhotoDimensions, error) {
		_, err := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader([]byte("jpeg bytes")))
		return store.PhotoDimensions{Width: 640, Height: 480}, err
	})
	if err != nil {
		t.Fatalf("photo assembly: %v", err)
	}
	assertPhotoFileMetadata(t, file)

	loaded, err := s.FilesByIDs(ctx, []int64{file.ID})
	if err != nil {
		t.Fatalf("load photo metadata: %v", err)
	}
	stored, ok := loaded[file.ID]
	if !ok {
		t.Fatalf("photo %d missing from stored file metadata", file.ID)
	}
	assertPhotoFileMetadata(t, stored)
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() }) //nolint:errcheck // best-effort close
	loaded, err = reopened.FilesByIDs(ctx, []int64{file.ID})
	if err != nil {
		t.Fatalf("reload photo metadata after restart: %v", err)
	}
	got, ok := loaded[file.ID]
	if !ok {
		t.Fatalf("photo %d missing after store recreation", file.ID)
	}
	assertPhotoFileMetadata(t, got)
	if got.AccessHash != file.AccessHash || got.Size != file.Size || got.MimeType != file.MimeType || got.FileName != file.FileName {
		t.Fatalf("photo metadata changed after restart: before %+v, after %+v", file, got)
	}
}

func TestPhotoAssemblyRejectsInvalidDimensionsWithoutStoring(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	uploader := mustUser(t, s, "+15559101003")
	var fileIDs []int64
	for _, dimensions := range []store.PhotoDimensions{
		{Width: 0, Height: 480},
		{Width: 640, Height: 0},
		{Width: 10001, Height: 1},
		{Width: 1, Height: 10001},
		{Width: 10001, Height: 10001},
		{Width: 9000, Height: 1001},
		{Width: 8400, Height: 400},
		{Width: 4097, Height: 4096},
	} {
		var allocated store.File
		_, err := s.AllocateAndCompletePhotoFile(ctx, uploader.ID, 10, "image/jpeg", "bad.jpg", bigQuota, func(file store.File) (store.PhotoDimensions, error) {
			allocated = file
			fileIDs = append(fileIDs, file.ID)
			return dimensions, nil
		})
		if !errors.Is(err, store.ErrInvalidPhotoDimensions) {
			t.Errorf("dimensions %+v: error = %v, want ErrInvalidPhotoDimensions", dimensions, err)
		}
		if allocated.ID == 0 {
			t.Errorf("dimensions %+v did not reach assembly", dimensions)
			continue
		}
		if files, loadErr := s.FilesByIDs(ctx, []int64{allocated.ID}); loadErr != nil {
			t.Fatalf("load invalid photo: %v", loadErr)
		} else if _, ok := files[allocated.ID]; ok {
			t.Errorf("invalid dimensions %+v became stored photo", dimensions)
		}
		if _, err := s.FileForDownload(ctx, allocated.ID, allocated.AccessHash, uploader.ID); !errors.Is(err, store.ErrFileNotFound) {
			t.Errorf("invalid dimensions %+v download error = %v, want ErrFileNotFound", dimensions, err)
		}
	}
	counts := sweep(t, s, future())
	if counts.UnassembledErased != len(fileIDs) {
		t.Fatalf("invalid photo erasure counts = %+v, want %d unassembled rows erased", counts, len(fileIDs))
	}
}

func TestFailedPhotoAssemblyRemainsUnstoredAndReclaimable(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	uploader := mustUser(t, s, "+15559101004")
	putErr := errors.New("blob write failed")
	var fileID int64
	var accessHash int64
	_, err := s.AllocateAndCompletePhotoFile(ctx, uploader.ID, 10, "image/jpeg", "failed.jpg", bigQuota, func(file store.File) (store.PhotoDimensions, error) {
		fileID = file.ID
		accessHash = file.AccessHash
		return store.PhotoDimensions{Width: 640, Height: 480}, putErr
	})
	if !errors.Is(err, putErr) {
		t.Fatalf("photo assembly error = %v, want blob write failure", err)
	}
	if fileID == 0 {
		t.Fatal("failed photo assembly did not allocate a reclaimable row")
	}
	if files, err := s.FilesByIDs(ctx, []int64{fileID}); err != nil {
		t.Fatalf("load failed photo: %v", err)
	} else if _, ok := files[fileID]; ok {
		t.Fatalf("failed photo %d is visible as stored", fileID)
	}
	if _, err := s.FileForDownload(ctx, fileID, accessHash, uploader.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("download failed photo: error = %v, want ErrFileNotFound", err)
	}

	counts := sweep(t, s, future())
	if counts.UnassembledErased != 1 {
		t.Fatalf("failed photo erasure counts = %+v, want one unassembled row erased", counts)
	}
	if ids, err := s.ExistingFileIDs(ctx, []int64{fileID}); err != nil {
		t.Fatalf("check erased photo row: %v", err)
	} else if _, exists := ids[fileID]; exists {
		t.Fatalf("failed photo row %d survived erasure", fileID)
	}
}

func TestMediaErasureSkipsClaimedPhotoAssembly(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	uploader := mustUser(t, s, "+15559101006")
	claimed := make(chan int64, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAssembly := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAssembly()
	store.SetAssemblyClaimHook(s, func(fileID int64) {
		claimed <- fileID
		<-release
	})
	defer store.SetAssemblyClaimHook(s, nil)

	type assemblyResult struct {
		file store.File
		err  error
	}
	done := make(chan assemblyResult, 1)
	go func() {
		file, err := s.AllocateAndCompletePhotoFile(ctx, uploader.ID, 10, "image/jpeg", "claimed.jpg", bigQuota, func(store.File) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{Width: 640, Height: 480}, nil
		})
		done <- assemblyResult{file: file, err: err}
	}()

	var fileID int64
	select {
	case fileID = <-claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("photo assembly did not reach its held claim")
	}
	counts, err := s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("media erasure sweep: %v", err)
	}
	if counts.UnassembledContended != 1 || counts.UnassembledErased != 0 {
		t.Fatalf("media erasure counts = %+v, want one claimed photo retained", counts)
	}
	if files, err := s.FilesByIDs(ctx, []int64{fileID}); err != nil {
		t.Fatalf("load claimed photo: %v", err)
	} else if _, ok := files[fileID]; ok {
		t.Fatalf("claimed photo %d is visible before assembly completion", fileID)
	}
	releaseAssembly()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("photo assembly after erasure: %v", result.err)
		}
		if result.file.ID != fileID || !result.file.Stored || result.file.Kind != store.FileKindPhoto {
			t.Fatalf("completed photo = %+v, want stored photo %d", result.file, fileID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("photo assembly did not complete after releasing its claim")
	}
}

func assertPhotoFileMetadata(t *testing.T, file store.File) {
	t.Helper()
	if file.Kind != store.FileKindPhoto {
		t.Errorf("file kind = %q, want photo", file.Kind)
	}
	if file.Width != 640 {
		t.Errorf("photo width = %d, want 640", file.Width)
	}
	if file.Height != 480 {
		t.Errorf("photo height = %d, want 480", file.Height)
	}
	if !slices.Equal(file.SubtypeRights, []string{"send_photos"}) {
		t.Errorf("photo subtype rights = %v, want [send_photos]", file.SubtypeRights)
	}
	if !file.Stored {
		t.Error("completed photo is not stored")
	}
}
