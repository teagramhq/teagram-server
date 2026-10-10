package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const photoDerivativesMigration = "20261010000071_photo_derivatives.sql"

type photoDerivativeInsert struct {
	mWidth   any
	mHeight  any
	mSize    any
	mBytes   []byte
	stripped []byte
}

func insertPhotoDerivative(ctx context.Context, conn *pgx.Conn, fileID int64, row photoDerivativeInsert) error {
	_, err := conn.Exec(ctx, `
		INSERT INTO photo_derivatives (file_id, m_width, m_height, m_size, m_bytes, stripped)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		fileID, row.mWidth, row.mHeight, row.mSize, row.mBytes, row.stripped)
	return err
}

func assertPhotoDerivativeViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
		t.Fatalf("error = %v, want check violation %s", err, constraint)
	}
}

func publishSyntheticPhoto(ctx context.Context, conn *pgx.Conn, fileID int64) error {
	result, err := conn.Exec(ctx, `
		UPDATE files
		SET stored = true, media_kind = 'photo', width = 640, height = 480,
		    subtype_rights = ARRAY['send_photos']::TEXT[]
		WHERE id = $1 AND stored = false`, fileID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errors.New("photo parent was not published")
	}
	return nil
}

func photoDerivativeConn(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close connection: %v", err)
		}
	})
	return conn
}

func TestPhotoDerivativesEnforceBoundsAndPresence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	uploader := mustUser(t, s, "+15559300001")
	conn := photoDerivativeConn(t, ctx, dsn)
	stripped := []byte{1, 40, 40}
	mBytes := []byte{0xff}

	// The schema accepts both a stripped-only row and the upper bounds for every
	// encoded derivative field.
	for _, row := range []photoDerivativeInsert{
		{stripped: stripped},
		{
			mWidth:   int32(1),
			mHeight:  int32(320),
			mSize:    int32(1),
			mBytes:   mBytes,
			stripped: append([]byte{1, 1, 40}, bytes.Repeat([]byte{0}, 2045)...),
		},
		{
			mWidth:   int32(320),
			mHeight:  int32(320),
			mSize:    int32(65536),
			mBytes:   bytes.Repeat([]byte{0xff}, 65536),
			stripped: stripped,
		},
	} {
		file := allocate(t, s, uploader.ID, 10)
		if err := insertPhotoDerivative(ctx, conn, file.ID, row); err != nil {
			t.Fatalf("insert valid derivative row for file %d: %v", file.ID, err)
		}
	}

	tooLargeM := bytes.Repeat([]byte{0xff}, 65537)
	tooLargeStripped := append([]byte{1, 40, 40}, bytes.Repeat([]byte{0}, 2046)...)
	for _, tc := range []struct {
		name       string
		row        photoDerivativeInsert
		constraint string
	}{
		{
			name: "partial m fields",
			row: photoDerivativeInsert{
				mWidth: int32(320), stripped: stripped,
			},
			constraint: "photo_derivatives_m_fields_present",
		},
		{
			name: "m size differs from bytes",
			row: photoDerivativeInsert{
				mWidth: int32(320), mHeight: int32(160), mSize: int32(2), mBytes: mBytes, stripped: stripped,
			},
			constraint: "photo_derivatives_m_size_valid",
		},
		{
			name: "m bytes are empty",
			row: photoDerivativeInsert{
				mWidth: int32(320), mHeight: int32(160), mSize: int32(0), mBytes: []byte{}, stripped: stripped,
			},
			constraint: "photo_derivatives_m_size_valid",
		},
		{
			name: "m bytes exceed limit",
			row: photoDerivativeInsert{
				mWidth: int32(320), mHeight: int32(160), mSize: int32(65537), mBytes: tooLargeM, stripped: stripped,
			},
			constraint: "photo_derivatives_m_size_valid",
		},
		{
			name: "m short side is zero",
			row: photoDerivativeInsert{
				mWidth: int32(320), mHeight: int32(0), mSize: int32(1), mBytes: mBytes, stripped: stripped,
			},
			constraint: "photo_derivatives_m_dimensions_valid",
		},
		{
			name: "m long side is not 320",
			row: photoDerivativeInsert{
				mWidth: int32(319), mHeight: int32(160), mSize: int32(1), mBytes: mBytes, stripped: stripped,
			},
			constraint: "photo_derivatives_m_dimensions_valid",
		},
		{
			name: "m dimension exceeds 320",
			row: photoDerivativeInsert{
				mWidth: int32(321), mHeight: int32(320), mSize: int32(1), mBytes: mBytes, stripped: stripped,
			},
			constraint: "photo_derivatives_m_dimensions_valid",
		},
		{name: "stripped shorter than prefix and dimensions", row: photoDerivativeInsert{stripped: []byte{1, 40}}, constraint: "photo_derivatives_stripped_valid"},
		{name: "stripped exceeds limit", row: photoDerivativeInsert{stripped: tooLargeStripped}, constraint: "photo_derivatives_stripped_valid"},
		{name: "stripped prefix differs", row: photoDerivativeInsert{stripped: []byte{0, 40, 40}}, constraint: "photo_derivatives_stripped_valid"},
		{name: "stripped height is zero", row: photoDerivativeInsert{stripped: []byte{1, 0, 40}}, constraint: "photo_derivatives_stripped_valid"},
		{name: "stripped width exceeds 40", row: photoDerivativeInsert{stripped: []byte{1, 40, 41}}, constraint: "photo_derivatives_stripped_valid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := allocate(t, s, uploader.ID, 10)
			err := insertPhotoDerivative(ctx, conn, file.ID, tc.row)
			assertPhotoDerivativeViolation(t, err, tc.constraint)
		})
	}

	file := allocate(t, s, uploader.ID, 10)
	err := insertPhotoDerivative(ctx, conn, file.ID, photoDerivativeInsert{})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != "stripped" {
		t.Fatalf("missing stripped bytes error = %v, want NOT NULL on stripped", err)
	}
}

func TestPhotoDerivativesFinalizeOnPublishAndKeepLegacyPhotosOriginalOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	uploader := mustUser(t, s, "+15559300002")
	conn := photoDerivativeConn(t, ctx, dsn)

	derivativeFile := allocate(t, s, uploader.ID, 11)
	if err := insertPhotoDerivative(ctx, conn, derivativeFile.ID, photoDerivativeInsert{
		mWidth: int32(320), mHeight: int32(240), mSize: int32(1), mBytes: []byte{0xff}, stripped: []byte{1, 40, 30},
	}); err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	if err := publishSyntheticPhoto(ctx, conn, derivativeFile.ID); err != nil {
		t.Fatalf("publish photo: %v", err)
	}

	if _, err := conn.Exec(ctx, `UPDATE photo_derivatives SET stripped = $2 WHERE file_id = $1`, derivativeFile.ID, []byte{1, 20, 20}); err == nil {
		t.Fatal("update of published derivatives succeeded")
	} else {
		assertPhotoDerivativeViolation(t, err, "photo_derivatives_immutable")
	}
	if _, err := conn.Exec(ctx, `DELETE FROM photo_derivatives WHERE file_id = $1`, derivativeFile.ID); err == nil {
		t.Fatal("independent delete of published derivatives succeeded")
	} else {
		assertPhotoDerivativeViolation(t, err, "photo_derivatives_immutable")
	}

	absenceFile := allocate(t, s, uploader.ID, 12)
	if err := publishSyntheticPhoto(ctx, conn, absenceFile.ID); err != nil {
		t.Fatalf("publish photo with no derivatives: %v", err)
	}
	if err := insertPhotoDerivative(ctx, conn, absenceFile.ID, photoDerivativeInsert{stripped: []byte{1, 40, 30}}); err == nil {
		t.Fatal("late insert after derivative absence was finalized succeeded")
	} else {
		assertPhotoDerivativeViolation(t, err, "photo_derivatives_parent_unstored")
	}

	legacy, err := s.AllocateAndCompletePhotoFile(ctx, uploader.ID, 11, "image/jpeg", "legacy.jpg", bigQuota, func(file store.File) (store.PhotoDimensions, error) {
		_, err := store.BlobsOf(s).Put(ctx, blob.Key(file.ID), bytes.NewReader([]byte("original")))
		return store.PhotoDimensions{Width: 640, Height: 480}, err
	})
	if err != nil {
		t.Fatalf("complete legacy photo: %v", err)
	}
	files, err := s.FilesByIDs(ctx, []int64{legacy.ID})
	if err != nil {
		t.Fatalf("load legacy photo metadata: %v", err)
	}
	got, ok := files[legacy.ID]
	if !ok || got.Kind != store.FileKindPhoto || got.Size != 11 || got.MimeType != "image/jpeg" || got.FileName != "legacy.jpg" || got.Width != 640 || got.Height != 480 {
		t.Fatalf("legacy photo metadata = %+v, present %v", got, ok)
	}
	var hasDerivative bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM photo_derivatives WHERE file_id = $1)`, legacy.ID).Scan(&hasDerivative); err != nil {
		t.Fatalf("check legacy derivative: %v", err)
	}
	if hasDerivative {
		t.Fatalf("ordinary photo assembly wrote derivative metadata for file %d", legacy.ID)
	}
}

func TestPhotoQuotaCountsPersistedDerivativeBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	uploader := mustUser(t, s, "+15559300020")
	conn := photoDerivativeConn(t, ctx, dsn)
	file := allocate(t, s, uploader.ID, 100)
	if err := insertPhotoDerivative(ctx, conn, file.ID, photoDerivativeInsert{
		mWidth: int32(320), mHeight: int32(320), mSize: int32(20), mBytes: bytes.Repeat([]byte{0xff}, 20), stripped: []byte{1, 1, 1, 0xff},
	}); err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	if err := publishSyntheticPhoto(ctx, conn, file.ID); err != nil {
		t.Fatalf("publish photo: %v", err)
	}
	if _, err := s.AllocateFile(ctx, uploader.ID, 1, "text/plain", "over-quota.txt", 124); !errors.Is(err, store.ErrStorageQuota) {
		t.Fatalf("allocation after 24 derivative bytes = %v, want ErrStorageQuota", err)
	}
	if _, err := s.AllocateFile(ctx, uploader.ID, 1, "text/plain", "at-quota.txt", 125); err != nil {
		t.Fatalf("allocation at exact derivative-inclusive quota: %v", err)
	}
}

func TestPhotoAssemblyPublishesDerivativesWithStoredFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := testBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // best-effort close
	uploader := mustUser(t, s, "+15559300021")
	conn := photoDerivativeConn(t, ctx, dsn)
	original := []byte("original JPEG bytes")
	mBytes := []byte{0xff, 0xd8, 0xff, 0xd9}
	stripped := []byte{1, 40, 40, 0xff, 0xd8, 0xff, 0xd9}
	file, derivativesStored, err := s.AllocateAndCompletePhotoFileWithDerivatives(
		ctx, uploader.ID, int64(len(original)), "image/jpeg", "photo.jpg", bigQuota,
		func(file store.File) (store.PhotoAssembly, error) {
			_, putErr := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(original))
			return store.PhotoAssembly{
				Dimensions:  store.PhotoDimensions{Width: 640, Height: 480},
				Derivatives: &store.PhotoDerivativeInput{MWidth: 320, MHeight: 320, MBytes: mBytes, Stripped: stripped},
			}, putErr
		},
	)
	if err != nil {
		t.Fatalf("complete photo assembly: %v", err)
	}
	if !derivativesStored || file.PhotoDerivatives == nil || file.PhotoDerivatives.MSize != len(mBytes) || !bytes.Equal(file.PhotoDerivatives.Stripped, stripped) {
		t.Fatalf("published photo = %+v, derivatives stored %v", file, derivativesStored)
	}
	var stored, hasDerivatives bool
	if err := conn.QueryRow(ctx, "SELECT stored, EXISTS (SELECT 1 FROM photo_derivatives WHERE file_id = $1) FROM files WHERE id = $1", file.ID).Scan(&stored, &hasDerivatives); err != nil {
		t.Fatalf("read publication state: %v", err)
	}
	if !stored || !hasDerivatives {
		t.Fatalf("publication state stored/derivatives = %v/%v, want both committed", stored, hasDerivatives)
	}
}

func TestPhotoAssemblyFallsBackWhenDerivativeInsertRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := testBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // best-effort close
	uploader := mustUser(t, s, "+15559300023")
	conn := photoDerivativeConn(t, ctx, dsn)
	if _, err := conn.Exec(ctx, `
CREATE FUNCTION reject_test_photo_derivative_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'test rejects photo derivative insert';
END;
$$;
CREATE TRIGGER reject_test_photo_derivative_insert
BEFORE INSERT ON photo_derivatives
FOR EACH ROW EXECUTE FUNCTION reject_test_photo_derivative_insert();`); err != nil {
		t.Fatalf("install derivative insert rejection: %v", err)
	}

	original := []byte("original JPEG bytes")
	file, derivativesStored, err := s.AllocateAndCompletePhotoFileWithDerivatives(
		ctx, uploader.ID, int64(len(original)), "image/jpeg", "photo.jpg", bigQuota,
		func(file store.File) (store.PhotoAssembly, error) {
			_, putErr := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(original))
			return store.PhotoAssembly{
				Dimensions:  store.PhotoDimensions{Width: 640, Height: 480},
				Derivatives: &store.PhotoDerivativeInput{MWidth: 320, MHeight: 320, MBytes: []byte{0xff}, Stripped: []byte{1, 40, 40}},
			}, putErr
		},
	)
	if err != nil {
		t.Fatalf("complete photo assembly after derivative rejection: %v", err)
	}
	if !file.Stored || derivativesStored || file.PhotoDerivatives != nil {
		t.Fatalf("published photo = %+v, derivatives stored %v, want original-only", file, derivativesStored)
	}
	var stored, hasDerivatives bool
	if err := conn.QueryRow(ctx, "SELECT stored, EXISTS (SELECT 1 FROM photo_derivatives WHERE file_id = $1) FROM files WHERE id = $1", file.ID).Scan(&stored, &hasDerivatives); err != nil {
		t.Fatalf("read publication state after derivative rejection: %v", err)
	}
	if !stored || hasDerivatives {
		t.Fatalf("publication state stored/derivatives = %v/%v, want stored original with no derivative", stored, hasDerivatives)
	}
	got, err := blobs.ReadAt(ctx, blob.Key(file.ID), 0, int64(len(original)))
	if err != nil {
		t.Fatalf("read original bytes after derivative rejection: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("original bytes = %q, want %q", got, original)
	}
}

func TestFailedPhotoPublicationRollsBackCandidateDerivatives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := testBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // best-effort close
	uploader := mustUser(t, s, "+15559300022")
	conn := photoDerivativeConn(t, ctx, dsn)
	if _, err := conn.Exec(ctx, "CREATE FUNCTION reject_test_photo_publication() RETURNS trigger\nLANGUAGE plpgsql AS $$\nBEGIN\n    IF OLD.stored = false AND NEW.stored = true THEN\n        RAISE EXCEPTION 'test rejects photo publication';\n    END IF;\n    RETURN NEW;\nEND;\n$$;\nCREATE TRIGGER reject_test_photo_publication\nBEFORE UPDATE OF stored ON files\nFOR EACH ROW EXECUTE FUNCTION reject_test_photo_publication();"); err != nil {
		t.Fatalf("install publication rejection: %v", err)
	}
	original := []byte("original JPEG bytes")
	var allocatedID int64
	file, derivativesStored, err := s.AllocateAndCompletePhotoFileWithDerivatives(
		ctx, uploader.ID, int64(len(original)), "image/jpeg", "photo.jpg", bigQuota,
		func(file store.File) (store.PhotoAssembly, error) {
			allocatedID = file.ID
			_, putErr := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(original))
			return store.PhotoAssembly{
				Dimensions:  store.PhotoDimensions{Width: 640, Height: 480},
				Derivatives: &store.PhotoDerivativeInput{MWidth: 320, MHeight: 320, MBytes: []byte{0xff}, Stripped: []byte{1, 40, 40}},
			}, putErr
		},
	)
	if err == nil || derivativesStored || file.Stored || allocatedID == 0 {
		t.Fatalf("rejected publication = file %+v, derivatives stored %v, allocated %d, err %v", file, derivativesStored, allocatedID, err)
	}
	var stored, hasDerivatives bool
	if err := conn.QueryRow(ctx, "SELECT stored, EXISTS (SELECT 1 FROM photo_derivatives WHERE file_id = $1) FROM files WHERE id = $1", allocatedID).Scan(&stored, &hasDerivatives); err != nil {
		t.Fatalf("read failed publication state: %v", err)
	}
	if stored || hasDerivatives {
		t.Fatalf("failed publication left stored/derivatives = %v/%v, want neither", stored, hasDerivatives)
	}
}

func TestPhotoDerivativeMetadataHydratesAcrossStoreRecreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := testBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // best-effort close
	uploader := mustUser(t, s, "+15559300009")
	file := allocate(t, s, uploader.ID, 11)
	conn := photoDerivativeConn(t, ctx, dsn)
	stripped := []byte{1, 20, 30, 0xaa}
	if err := insertPhotoDerivative(ctx, conn, file.ID, photoDerivativeInsert{
		mWidth: int32(320), mHeight: int32(320), mSize: int32(4), mBytes: []byte{0xff, 0xd8, 0xff, 0xd9}, stripped: stripped,
	}); err != nil {
		t.Fatalf("insert derivative: %v", err)
	}
	if err := publishSyntheticPhoto(ctx, conn, file.ID); err != nil {
		t.Fatalf("publish photo: %v", err)
	}

	loaded, err := s.FilesByIDs(ctx, []int64{file.ID})
	if err != nil {
		t.Fatalf("load derivative metadata: %v", err)
	}
	assertHydratedPhotoDerivative(t, loaded[file.ID], stripped)
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
		t.Fatalf("reload derivative metadata after restart: %v", err)
	}
	assertHydratedPhotoDerivative(t, loaded[file.ID], stripped)
}

func assertHydratedPhotoDerivative(t *testing.T, file store.File, stripped []byte) {
	t.Helper()
	derivatives := file.PhotoDerivatives
	if derivatives == nil || derivatives.MWidth != 320 || derivatives.MHeight != 320 || derivatives.MSize != 4 || !bytes.Equal(derivatives.Stripped, stripped) {
		t.Fatalf("photo derivatives = %+v, want 320x320 m size 4 and the stripped preview", derivatives)
	}
}

func TestPhotoDerivativeMigrationPreservesExistingPhotoMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, _ := profilePhotoDatabaseBefore(t, ctx, photoDerivativesMigration)
	owner := profilePhotoUser(t, ctx, conn)
	var fileID int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored, media_kind, width, height, subtype_rights)
		VALUES ($1, 55, 12345, 'image/jpeg', 'existing.jpg', true, 'photo', 640, 480, ARRAY['send_photos']::TEXT[])
		RETURNING id`, owner).Scan(&fileID); err != nil {
		t.Fatalf("seed existing photo: %v", err)
	}
	if _, err := conn.Exec(ctx, profilePhotoReadMigration(t, photoDerivativesMigration)); err != nil {
		t.Fatalf("apply derivative migration to existing schema: %v", err)
	}

	var size int64
	var mimeType, fileName, mediaKind string
	var stored bool
	var width, height int32
	if err := conn.QueryRow(ctx, `SELECT size, mime_type, file_name, stored, media_kind, width, height FROM files WHERE id = $1`, fileID).
		Scan(&size, &mimeType, &fileName, &stored, &mediaKind, &width, &height); err != nil {
		t.Fatalf("read existing photo after migration: %v", err)
	}
	if size != 12345 || mimeType != "image/jpeg" || fileName != "existing.jpg" || !stored || mediaKind != "photo" || width != 640 || height != 480 {
		t.Fatalf("existing photo changed after migration: size=%d mime=%q name=%q stored=%v kind=%q dimensions=%dx%d",
			size, mimeType, fileName, stored, mediaKind, width, height)
	}
	var derivativeRows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM photo_derivatives`).Scan(&derivativeRows); err != nil {
		t.Fatalf("count derivative rows after migration: %v", err)
	}
	if derivativeRows != 0 {
		t.Fatalf("migration created %d derivative rows for existing photos", derivativeRows)
	}
}

func TestPhotoDerivativeRowsCascadeThroughBothMediaErasurePaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	uploader := mustUser(t, s, "+15559300003")
	conn := photoDerivativeConn(t, ctx, dsn)

	stored := allocate(t, s, uploader.ID, 11)
	if err := insertPhotoDerivative(ctx, conn, stored.ID, photoDerivativeInsert{stripped: []byte{1, 40, 30}}); err != nil {
		t.Fatalf("insert stored candidate derivative: %v", err)
	}
	if _, err := store.BlobsOf(s).Put(ctx, blob.Key(stored.ID), bytes.NewReader([]byte("original"))); err != nil {
		t.Fatalf("put stored candidate blob: %v", err)
	}
	if err := publishSyntheticPhoto(ctx, conn, stored.ID); err != nil {
		t.Fatalf("publish stored candidate: %v", err)
	}

	unassembled := allocate(t, s, uploader.ID, 12)
	if err := insertPhotoDerivative(ctx, conn, unassembled.ID, photoDerivativeInsert{stripped: []byte{1, 40, 30}}); err != nil {
		t.Fatalf("insert unassembled candidate derivative: %v", err)
	}
	if _, err := store.BlobsOf(s).Put(ctx, blob.Key(unassembled.ID), bytes.NewReader([]byte("partial"))); err != nil {
		t.Fatalf("put unassembled candidate blob: %v", err)
	}

	counts := sweep(t, s, future())
	if counts.Erased != 1 || counts.UnassembledErased != 1 {
		t.Fatalf("erasure counts = %+v, want one stored and one unassembled row erased", counts)
	}
	for _, fileID := range []int64{stored.ID, unassembled.ID} {
		var present bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM photo_derivatives WHERE file_id = $1)`, fileID).Scan(&present); err != nil {
			t.Fatalf("check derivative cascade for file %d: %v", fileID, err)
		}
		if present {
			t.Errorf("photo derivative row for erased file %d survived", fileID)
		}
	}
}
