package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// The two migrations that carry the inert profile-photo foundations.
// They are named here because the tests below apply them one at a time against
// databases pinned just before them: the ownership backstop has to be provable
// as a non-blocking build on a populated files table, which is only observable
// on a database that does not already have it.
const (
	profileOwnershipIndexMigration = "20261008000068_files_owner_ownership_backstop.sql"
	profileGallerySchemaMigration  = "20261008000069_profile_photo_gallery.sql"
)

// profilePhotoTables are the inert tables this slice adds. None
// of them has a writer: the guarantees below are constraints, and the only thing
// under test is whether the schema holds them.
var profilePhotoTables = []string{
	"user_photos",
	"profile_photo_state",
	"profile_upload_receipt",
	"profile_delete_operation",
}

func TestProfilePhotoSchemaLandsInertOnFreshSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fresh schema: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	for _, table := range profilePhotoTables {
		var present bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&present); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !present {
			t.Errorf("table %s missing from the freshly migrated schema", table)
		}
	}

	// A concurrent build that failed leaves an INVALID index behind, and Postgres
	// then ignores it in silence. The gallery foreign key cannot be
	// created against such an index, so the apply fails — but only if validity is
	// actually checked somewhere, which is this assertion.
	var valid, ready bool
	if err := conn.QueryRow(ctx, `
		SELECT x.indisvalid, x.indisready
		FROM pg_index x
		JOIN pg_class c ON c.oid = x.indexrelid
		WHERE c.relname = 'files_id_uploader_id_key'
	`).Scan(&valid, &ready); err != nil {
		t.Fatalf("read ownership index state: %v", err)
	}
	if !valid || !ready {
		t.Errorf("files_id_uploader_id_key valid/ready = %v/%v, want true/true", valid, ready)
	}

	// The application shape still opens against the new schema: these tables are
	// additive, and no existing writer changed.
	opened, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store on fresh schema: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func TestProfilePhotoGalleryOwnershipBackstop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)

	owner := profilePhotoUser(t, ctx, conn)
	other := profilePhotoUser(t, ctx, conn)
	ownedFile := profilePhotoFile(t, ctx, conn, owner)
	foreignFile := profilePhotoFile(t, ctx, conn, other)
	secondOwnedFile := profilePhotoFile(t, ctx, conn, owner)

	// The accepted case: owner 1's gallery referencing file 1.
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, ownedFile, int64(101)); err != nil {
		t.Fatalf("insert own gallery entry: %v", err)
	}

	// The same structural backstop rejects a gallery entry for a file the owner
	// does not own. It is a foreign key, not a predicate: nothing a later writer
	// queries can turn it into a wrong answer.
	_, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, foreignFile, int64(102))
	if !profilePhotoViolation(err, "23503", "user_photos_file_owned_by_owner") {
		t.Fatalf("cross-owner gallery insert err = %v, want user_photos_file_owned_by_owner violation", err)
	}

	// A file has at most one gallery entry, so it can have at most one gallery
	// owner. With the ownership key in place, a second entry for one file is
	// already a primary-key duplicate, because that key pins user_id to the
	// file's uploader. The unique file id is the layer that keeps the rule when
	// the foreign key is not there, and the layer that makes the file-only probe
	// indexable at all, so it is asserted on its own: drop the foreign key on this
	// disposable database and a second owner for one file is still refused.
	if _, err := conn.Exec(ctx, `ALTER TABLE user_photos DROP CONSTRAINT user_photos_file_owned_by_owner`); err != nil {
		t.Fatalf("drop ownership foreign key: %v", err)
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, other, ownedFile, int64(103))
	if !profilePhotoViolation(err, "23505", "user_photos_file_id_key") {
		t.Fatalf("second owner for one file err = %v, want user_photos_file_id_key violation", err)
	}

	// One client upload id is one gallery entry per owner: two parallel uploads
	// of the same id cannot both land, so they cannot both be charged.
	_, err = conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, secondOwnedFile, int64(101))
	if !profilePhotoViolation(err, "23505", "user_photos_user_id_client_file_id_key") {
		t.Fatalf("reused client file id err = %v, want user_photos_user_id_client_file_id_key violation", err)
	}
}

func TestProfilePhotoGalleryReferenceRestrictsFileDeletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)

	owner := profilePhotoUser(t, ctx, conn)
	fileID := profilePhotoFile(t, ctx, conn, owner)
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, fileID, int64(201)); err != nil {
		t.Fatalf("insert gallery entry: %v", err)
	}

	// Forced deletion of a referenced file, bypassing every application path.
	// RESTRICT is the eraser's backstop: a file a gallery still shows stays.
	_, err := conn.Exec(ctx, `DELETE FROM files WHERE id = $1`, fileID)
	if !profilePhotoViolation(err, "23503", "user_photos_file_owned_by_owner") {
		t.Fatalf("forced delete of referenced file err = %v, want user_photos_file_owned_by_owner violation", err)
	}

	var remaining int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM files WHERE id = $1`, fileID).Scan(&remaining); err != nil {
		t.Fatalf("count file rows: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("files rows for %d = %d after a restricted delete, want 1", fileID, remaining)
	}
}

func TestProfilePhotoCurrentSelectionNamesOnlyOwnGalleryEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)

	owner := profilePhotoUser(t, ctx, conn)
	other := profilePhotoUser(t, ctx, conn)
	galleryFile := profilePhotoFile(t, ctx, conn, owner)
	unlistedFile := profilePhotoFile(t, ctx, conn, owner)
	foreignFile := profilePhotoFile(t, ctx, conn, other)
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, galleryFile, int64(301)); err != nil {
		t.Fatalf("insert gallery entry: %v", err)
	}

	// An empty avatar is the NULL pointer, and a MATCH SIMPLE composite key
	// leaves it unchecked, which is what makes the empty state representable.
	if _, err := conn.Exec(ctx, `
		INSERT INTO profile_photo_state (user_id, current_file_id, mutation_revision)
		VALUES ($1, NULL, 0)
	`, owner); err != nil {
		t.Fatalf("insert empty selection state: %v", err)
	}

	// A pointer to one of the owner's own files that is not in their gallery is
	// rejected: current names a live gallery entry, not any owned file.
	_, err := conn.Exec(ctx, `UPDATE profile_photo_state SET current_file_id = $2 WHERE user_id = $1`,
		owner, unlistedFile)
	if !profilePhotoViolation(err, "23503", "profile_photo_state_current_is_own_gallery_entry") {
		t.Fatalf("pointer outside gallery err = %v, want profile_photo_state_current_is_own_gallery_entry violation", err)
	}

	// A pointer to another account's file is rejected the same way.
	_, err = conn.Exec(ctx, `UPDATE profile_photo_state SET current_file_id = $2 WHERE user_id = $1`,
		owner, foreignFile)
	if !profilePhotoViolation(err, "23503", "profile_photo_state_current_is_own_gallery_entry") {
		t.Fatalf("pointer to foreign file err = %v, want profile_photo_state_current_is_own_gallery_entry violation", err)
	}

	// The accepted selection: an entry in this owner's gallery.
	if _, err := conn.Exec(ctx, `
		UPDATE profile_photo_state SET current_file_id = $2, mutation_revision = 4 WHERE user_id = $1
	`, owner, galleryFile); err != nil {
		t.Fatalf("select own gallery entry: %v", err)
	}

	// RESTRICT is the interlock that makes the writer clear the pointer before it
	// deletes the entry the pointer names. The order is not a convention.
	_, err = conn.Exec(ctx, `DELETE FROM user_photos WHERE user_id = $1 AND file_id = $2`, owner, galleryFile)
	if !profilePhotoViolation(err, "23503", "profile_photo_state_current_is_own_gallery_entry") {
		t.Fatalf("delete pointed gallery entry err = %v, want profile_photo_state_current_is_own_gallery_entry violation", err)
	}

	// Clear first, then delete: the same transaction order the gallery writer
	// will have to follow.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin clear-and-delete: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after commit
	if _, err := tx.Exec(ctx, `
		UPDATE profile_photo_state
		SET current_file_id = NULL, mutation_revision = mutation_revision + 1
		WHERE user_id = $1
	`, owner); err != nil {
		t.Fatalf("clear current selection: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_photos WHERE user_id = $1 AND file_id = $2`, owner, galleryFile); err != nil {
		t.Fatalf("delete cleared gallery entry: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit clear-and-delete: %v", err)
	}
}

func TestProfilePhotoUploadReceiptDoesNotPinItsFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)

	owner := profilePhotoUser(t, ctx, conn)
	fileID := profilePhotoFile(t, ctx, conn, owner)
	if _, err := conn.Exec(ctx, `
		INSERT INTO profile_upload_receipt
		    (user_id, client_file_id, file_id, state, request_size, part_count, payload_digest, media_mode)
		VALUES ($1, $2, $3, 2, 4096, 2, decode(repeat('ab', 32), 'hex'), 'photo')
	`, owner, int64(401), fileID); err != nil {
		t.Fatalf("insert terminal receipt: %v", err)
	}

	// A receipt is retry metadata, not a live media reference. The file row it
	// names is otherwise unreferenced, so the eraser path deletes it, and the
	// receipt must not stop that.
	if _, err := conn.Exec(ctx, `DELETE FROM files WHERE id = $1`, fileID); err != nil {
		t.Fatalf("delete file referenced only by a receipt: %v", err)
	}

	// The receipt survives with its terminal state intact and a null file
	// reference, so a retry of that client id stays a uniform refusal rather
	// than a fresh upload of a deleted photo.
	var state int16
	var pinned bool
	if err := conn.QueryRow(ctx, `
		SELECT state, file_id IS NOT NULL
		FROM profile_upload_receipt
		WHERE user_id = $1 AND client_file_id = $2
	`, owner, int64(401)).Scan(&state, &pinned); err != nil {
		t.Fatalf("read receipt after file deletion: %v", err)
	}
	if state != 2 || pinned {
		t.Fatalf("receipt state/file-pinned = %d/%v after file deletion, want 2/false", state, pinned)
	}

	// The fingerprint columns are the reject-reuse contract, so they
	// have to be real: a bogus state or a truncated digest is refused.
	if _, err := conn.Exec(ctx, `
		INSERT INTO profile_upload_receipt
		    (user_id, client_file_id, file_id, state, request_size, part_count, payload_digest, media_mode)
		VALUES ($1, $2, NULL, 7, 1, 1, decode(repeat('cd', 32), 'hex'), 'photo')
	`, owner, int64(402)); !profilePhotoViolation(err, "23514", "profile_upload_receipt_state_check") {
		t.Fatalf("receipt with unknown state err = %v, want profile_upload_receipt_state_check violation", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO profile_upload_receipt
		    (user_id, client_file_id, file_id, state, request_size, part_count, payload_digest, media_mode)
		VALUES ($1, $2, NULL, 0, 1, 1, decode('ab', 'hex'), 'photo')
	`, owner, int64(403)); !profilePhotoViolation(err, "23514", "profile_upload_receipt_payload_digest_check") {
		t.Fatalf("receipt with short digest err = %v, want profile_upload_receipt_payload_digest_check violation", err)
	}
}

func TestProfilePhotoStateRevisionSurvivesEveryGalleryMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)

	owner := profilePhotoUser(t, ctx, conn)
	fileID := profilePhotoFile(t, ctx, conn, owner)
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, fileID, int64(501)); err != nil {
		t.Fatalf("insert gallery entry: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO profile_photo_state (user_id, current_file_id, mutation_revision)
		VALUES ($1, $2, 7)
	`, owner, fileID); err != nil {
		t.Fatalf("insert selection state: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO profile_upload_receipt
		    (user_id, client_file_id, file_id, state, request_size, part_count, payload_digest, media_mode)
		VALUES ($1, $2, $3, 1, 2048, 1, decode(repeat('ef', 32), 'hex'), 'photo')
	`, owner, int64(501), fileID); err != nil {
		t.Fatalf("insert complete receipt: %v", err)
	}

	// Empty the account's whole gallery and reclaim the file, the way a delete
	// plus a later erasure sweep would.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gallery clear: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after commit
	if _, err := tx.Exec(ctx, `
		UPDATE profile_photo_state SET current_file_id = NULL WHERE user_id = $1
	`, owner); err != nil {
		t.Fatalf("clear current selection: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_photos WHERE user_id = $1`, owner); err != nil {
		t.Fatalf("delete gallery entries: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM profile_upload_receipt WHERE user_id = $1`, owner); err != nil {
		t.Fatalf("terminalize receipts: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit gallery clear: %v", err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM files WHERE id = $1`, fileID); err != nil {
		t.Fatalf("reclaim unreferenced file: %v", err)
	}

	// The per-owner revision row is never compacted: an acknowledged deletion has
	// to stay acknowledged across a restore that predates the upload it cleared,
	// and that counter lives in this row. Nothing here removes it.
	var gallery, receipts int64
	var revision int64
	var current bool
	if err := conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM user_photos WHERE user_id = $1),
		       (SELECT count(*) FROM profile_upload_receipt WHERE user_id = $1),
		       mutation_revision,
		       current_file_id IS NOT NULL
		FROM profile_photo_state
		WHERE user_id = $1
	`, owner).Scan(&gallery, &receipts, &revision, &current); err != nil {
		t.Fatalf("read state after gallery clear: %v", err)
	}
	if gallery != 0 || receipts != 0 {
		t.Fatalf("gallery/receipt rows after clear = %d/%d, want 0/0", gallery, receipts)
	}
	if revision != 7 || current {
		t.Fatalf("state revision/current-set = %d/%v after clear, want 7/false", revision, current)
	}
}

func TestProfilePhotoGalleryIndexesServePagingAndReverseProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)

	owner := profilePhotoUser(t, ctx, conn)
	other := profilePhotoUser(t, ctx, conn)
	cursor := profilePhotoSeedInterleavedGallery(t, ctx, conn, owner, other, 5000)
	if _, err := conn.Exec(ctx, `ANALYZE user_photos`); err != nil {
		t.Fatalf("analyze gallery: %v", err)
	}

	// Gallery paging is descending file id with a max_id cursor, and it stays an
	// index seek: either the owner primary key scanned backwards, or the unique
	// file id scanned backwards with the owner as a filter. Which one the planner
	// picks depends on how gallery ids correlate with accounts, and both are
	// bounded. What must never appear is a sequential scan of the gallery or a
	// sort, which is what turns one page request into a full gallery read.
	paged := profilePhotoExplain(t, ctx, conn, `
		SELECT file_id FROM user_photos
		WHERE user_id = $1 AND file_id < $2
		ORDER BY file_id DESC
		LIMIT 50
	`, owner, cursor)
	if strings.Contains(paged, "Seq Scan on user_photos") || strings.Contains(paged, "Sort Method") ||
		!strings.Contains(paged, "using user_photos_") {
		t.Fatalf("gallery paging is not a bounded gallery-index seek:\n%s", paged)
	}

	// The contract that plan serves: the cursor excludes itself, the page comes
	// back strictly descending, and every entry on it is the caller's own file.
	page := profilePhotoPage(t, ctx, conn, owner, cursor, 50)
	if len(page) != 50 {
		t.Fatalf("gallery page size = %d, want 50", len(page))
	}
	for i, id := range page {
		if id >= cursor || (i > 0 && id >= page[i-1]) {
			t.Fatalf("gallery page %v is not strictly descending below cursor %d", page, cursor)
		}
	}
	var owned int64
	if err := conn.QueryRow(ctx, `
		SELECT count(*)
		FROM user_photos up
		JOIN files f ON f.id = up.file_id
		WHERE up.user_id = $1 AND up.file_id = ANY($2) AND f.uploader_id = up.user_id
	`, owner, page).Scan(&owned); err != nil {
		t.Fatalf("count owned page entries: %v", err)
	}
	if owned != int64(len(page)) {
		t.Fatalf("page entries owned by the caller = %d, want %d", owned, len(page))
	}

	// The reverse probe is the question the eraser asks per candidate file, and
	// the primary key cannot answer it because it leads with user_id. This is the
	// access path the retention arm plans against.
	probe := profilePhotoExplain(t, ctx, conn, `
		SELECT 1 FROM user_photos WHERE file_id = $1 LIMIT 1
	`, cursor)
	if !strings.Contains(probe, "user_photos_file_id_key") {
		t.Fatalf("reverse reference probe has no user_photos_file_id_key seek:\n%s", probe)
	}

	// The deletion-operation dedup lookup is the whole reason that table exists:
	// a retry has to find the operation that already ran. It rides the primary
	// key, so the per-owner bound on those rows needs no second index.
	profilePhotoSeedDeleteOperations(t, ctx, conn, owner, 500)
	if _, err := conn.Exec(ctx, `ANALYZE profile_delete_operation`); err != nil {
		t.Fatalf("analyze deletion operations: %v", err)
	}
	ops := profilePhotoExplain(t, ctx, conn, `
		SELECT operation_key FROM profile_delete_operation
		WHERE user_id = $1 AND auth_key_id = $2 AND session_id = $3 AND msg_id = $4
	`, owner, int64(4242), int64(1), int64(303))
	if !strings.Contains(ops, "profile_delete_operation_pkey") {
		t.Fatalf("deletion operation dedup lookup has no profile_delete_operation_pkey seek:\n%s", ops)
	}
}

func TestProfileFilesOwnershipBackstopBuildsBehindAnOpenWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, dbName := profilePhotoDatabaseBefore(t, ctx, profileOwnershipIndexMigration)

	owner := profilePhotoUser(t, ctx, conn)
	other := profilePhotoUser(t, ctx, conn)
	const seededFiles = 20000
	tag, err := conn.Exec(ctx, `
		INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored)
		SELECT $1, 1000000 + g, 1, 'application/octet-stream', 'fixture.bin', true
		FROM generate_series(1, $2) g
	`, owner, seededFiles)
	if err != nil {
		t.Fatalf("populate files: %v", err)
	}
	if tag.RowsAffected() != seededFiles {
		t.Fatalf("populated files rows = %d, want %d", tag.RowsAffected(), seededFiles)
	}

	// A hot-table write is open when the build starts. The build must wait for it
	// rather than miss its row, and the write must not be blocked by the build.
	writer, err := pgx.Connect(ctx, pgtest.DSNFrom(dbName))
	if err != nil {
		t.Fatalf("connect writer: %v", err)
	}
	defer func() { _ = writer.Close(ctx) }() //nolint:errcheck // best-effort close
	writerTx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin open write: %v", err)
	}
	var openWriteFileID int64
	if err := writerTx.QueryRow(ctx, `
		INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored)
		VALUES ($1, 990001, 1, 'application/octet-stream', 'open-write.bin', true)
		RETURNING id
	`, owner).Scan(&openWriteFileID); err != nil {
		t.Fatalf("insert in open write: %v", err)
	}

	build, err := pgx.Connect(ctx, pgtest.DSNFrom(dbName))
	if err != nil {
		t.Fatalf("connect builder: %v", err)
	}
	defer func() { _ = build.Close(ctx) }() //nolint:errcheck // best-effort close
	buildDone := profilePhotoBuildOwnershipIndex(ctx, build,
		profilePhotoReadMigration(t, profileOwnershipIndexMigration))

	profilePhotoWaitWhileBuilding(t, ctx, conn)

	// Ordinary writes keep committing while the build runs: ShareUpdateExclusive
	// does not conflict with the row lock an INSERT takes. This is the whole
	// point of CONCURRENTLY on a table every media upload writes to.
	late, err := pgx.Connect(ctx, pgtest.DSNFrom(dbName))
	if err != nil {
		t.Fatalf("connect late writer: %v", err)
	}
	defer func() { _ = late.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := late.Exec(ctx, `SET statement_timeout = '20s'`); err != nil {
		t.Fatalf("set late writer timeout: %v", err)
	}
	var lateFileID int64
	if err := late.QueryRow(ctx, `
		INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored)
		VALUES ($1, 990002, 1, 'application/octet-stream', 'late-write.bin', true)
		RETURNING id
	`, owner).Scan(&lateFileID); err != nil {
		t.Fatalf("write during concurrent build: %v", err)
	}

	select {
	case err := <-buildDone:
		t.Fatalf("concurrent build finished while a hot-table write was open (err=%v)", err)
	case <-time.After(2 * time.Second):
	}

	if err := writerTx.Commit(ctx); err != nil {
		t.Fatalf("commit open write: %v", err)
	}
	select {
	case err := <-buildDone:
		if err != nil {
			t.Fatalf("apply ownership backstop concurrently: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatalf("concurrent build did not finish after the open write committed")
	}

	var valid, ready bool
	if err := conn.QueryRow(ctx, `
		SELECT x.indisvalid, x.indisready
		FROM pg_index x
		JOIN pg_class c ON c.oid = x.indexrelid
		WHERE c.relname = 'files_id_uploader_id_key'
	`).Scan(&valid, &ready); err != nil {
		t.Fatalf("read ownership index state: %v", err)
	}
	if !valid || !ready {
		t.Fatalf("files_id_uploader_id_key valid/ready = %v/%v after concurrent build, want true/true", valid, ready)
	}

	// The gallery table now depends on that index, and the composite key is what
	// makes ownership structural. Both rows written during the build are
	// referencable, which is only true if the build captured them: a missed
	// tuple reads as a foreign-key violation on a file that exists.
	if _, err := conn.Exec(ctx, profilePhotoReadMigration(t, profileGallerySchemaMigration)); err != nil {
		t.Fatalf("apply gallery schema after the backstop: %v", err)
	}
	for _, fileID := range []int64{openWriteFileID, lateFileID} {
		if _, err := conn.Exec(ctx, `
			INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
		`, owner, fileID, fileID); err != nil {
			t.Fatalf("gallery entry for file %d written during the build: %v", fileID, err)
		}
	}

	// The control: the same insert for a file id that does not exist is rejected,
	// so the accepted rows above are the index answering, not a constraint that
	// stopped checking.
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, int64(999999999), int64(999999999)); !profilePhotoViolation(err, "23503", "user_photos_file_owned_by_owner") {
		t.Fatalf("gallery entry for a missing file err = %v, want user_photos_file_owned_by_owner violation", err)
	}

	// And the backstop holds on this upgraded database, not only on a fresh
	// one. The file here belongs to the other account and is in no gallery, so the
	// ownership key is the only thing that can refuse this row.
	otherFile := profilePhotoFile(t, ctx, conn, other)
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $3)
	`, owner, otherFile, int64(777777)); !profilePhotoViolation(err, "23503", "user_photos_file_owned_by_owner") {
		t.Fatalf("cross-owner gallery entry on the upgraded database err = %v, want user_photos_file_owned_by_owner violation", err)
	}
}

func TestProfileFilesOwnershipBackstopMigrationKeepsBothRunners(t *testing.T) {
	t.Parallel()
	body := profilePhotoReadMigration(t, profileOwnershipIndexMigration)

	// Exactly one statement: the pgtest harness applies each migration as one
	// simple query, and a multi-statement simple query is a transaction
	// block, which CREATE INDEX CONCURRENTLY refuses.
	statements := 0
	for chunk := range strings.SplitSeq(body, ";") {
		if profilePhotoStripComments(chunk) != "" {
			statements++
		}
	}
	if statements != 1 {
		t.Fatalf("ownership migration has %d statements, want exactly 1:\n%s", statements, body)
	}

	// The directive has to sit inside the file's leading comment block. A blank
	// line between the prose and the directive makes Atlas ignore it, and the
	// statement then runs wrapped in a transaction and fails.
	lines := strings.Split(body, "\n")
	directive := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "-- atlas:txmode none" {
			directive = i
			break
		}
	}
	if directive == -1 {
		t.Fatalf("ownership migration has no transaction directive:\n%s", body)
	}
	if directive == 0 || !strings.HasPrefix(strings.TrimSpace(lines[directive-1]), "--") {
		t.Fatalf("transaction directive at line %d is not inside the leading comment block", directive+1)
	}
	if directive+1 >= len(lines) || strings.TrimSpace(lines[directive+1]) != "" {
		t.Fatalf("transaction directive is not followed by a blank line:\n%s", body)
	}

	// Ordering is load-bearing: the gallery table's composite foreign key needs
	// the index this file builds, so a rename that sorts them the other way turns
	// the apply into a failure.
	if profileOwnershipIndexMigration >= profileGallerySchemaMigration {
		t.Fatalf("ownership index migration %s does not sort before gallery schema migration %s",
			profileOwnershipIndexMigration, profileGallerySchemaMigration)
	}
}

// profilePhotoConn returns a connection to a freshly migrated database with the
// first-user election closed, so these tests can write fixture rows directly.
// The schema's constraints are under test, not the store's user-creation path.
func profilePhotoConn(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := conn.Close(cleanupCtx); err != nil {
			t.Logf("close connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`); err != nil {
		t.Fatalf("close server administrator election: %v", err)
	}
	return conn
}

// profilePhotoDatabaseBefore creates a disposable database carrying every
// migration that sorts before boundary, and returns it with its database name so
// a second connection can write to it while a migration applies.
func profilePhotoDatabaseBefore(t *testing.T, ctx context.Context, boundary string) (*pgx.Conn, string) {
	t.Helper()
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close

	// G201 is not a risk here: the name is internally generated hex and Postgres
	// DDL cannot bind an identifier as a parameter.
	name := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		conn, err := pgx.Connect(cleanupCtx, pgtest.AdminDSN())
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }() //nolint:errcheck // best-effort close
		if _, err := conn.Exec(cleanupCtx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("cleanup drop %s: %v", name, err)
		}
	})

	conn, err := pgx.Connect(ctx, pgtest.DSNFrom(name))
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := conn.Close(cleanupCtx); err != nil {
			t.Logf("close %s: %v", name, err)
		}
	})

	entries, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= boundary {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply migration %s: %v", entry.Name(), err)
		}
	}
	if _, err := conn.Exec(ctx, `UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`); err != nil {
		t.Fatalf("close server administrator election: %v", err)
	}
	return conn, name
}

func profilePhotoUser(t *testing.T, ctx context.Context, conn *pgx.Conn) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx, `INSERT INTO users (phone) VALUES ($1) RETURNING id`,
		"+1555"+pgtest.RandomHex()).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func profilePhotoFile(t *testing.T, ctx context.Context, conn *pgx.Conn, uploaderID int64) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored)
		VALUES ($1, $2, 64, 'image/jpeg', 'photo.jpg', true)
		RETURNING id
	`, uploaderID, uploaderID*1000+7).Scan(&id); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	return id
}

// profilePhotoSeedInterleavedGallery creates total files owned alternately by
// the two accounts, with a gallery entry for each, and returns the largest file
// id as the paging cursor. Alternating owners is not decoration: with gallery ids
// clustered by account, the planner reads one account's page as a scan down the
// file-only index filtering the other account out, which is the plan this test
// must not accept.
func profilePhotoSeedInterleavedGallery(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID, otherID int64, total int) int64 {
	t.Helper()
	if _, err := conn.Exec(ctx, `
		INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored)
		SELECT CASE WHEN g % 2 = 0 THEN $1::bigint ELSE $2::bigint END, 3000000 + g, 64, 'image/jpeg', 'photo.jpg', true
		FROM generate_series(1, $3) g
	`, ownerID, otherID, total); err != nil {
		t.Fatalf("seed interleaved gallery files: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id)
		SELECT f.uploader_id, f.id, f.id
		FROM files f
		WHERE f.uploader_id IN ($1, $2)
	`, ownerID, otherID); err != nil {
		t.Fatalf("seed interleaved gallery entries: %v", err)
	}
	var largest int64
	if err := conn.QueryRow(ctx, `SELECT max(file_id) FROM user_photos`).Scan(&largest); err != nil {
		t.Fatalf("read largest gallery file id: %v", err)
	}
	return largest
}

// profilePhotoPage runs the gallery list the way the gallery lane will: a max_id
// cursor, descending file id, bounded limit.
func profilePhotoPage(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID, maxID int64, limit int) []int64 {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT file_id FROM user_photos
		WHERE user_id = $1 AND file_id < $2
		ORDER BY file_id DESC
		LIMIT $3
	`, ownerID, maxID, limit)
	if err != nil {
		t.Fatalf("page gallery: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan gallery page: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read gallery page: %v", err)
	}
	return ids
}

// profilePhotoSeedDeleteOperations fills one owner's dedup table so the plan
// assertion below describes a table the planner has statistics about, not an
// empty one it can seq scan for free.
func profilePhotoSeedDeleteOperations(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64, count int) {
	t.Helper()
	if _, err := conn.Exec(ctx, `
		INSERT INTO profile_delete_operation (user_id, auth_key_id, session_id, msg_id, operation_key)
		SELECT $1, 4242, g, 2 * g, decode(substr(md5(g::text), 1, 32), 'hex')
		FROM generate_series(1, $2) g
	`, ownerID, count); err != nil {
		t.Fatalf("seed deletion operations: %v", err)
	}
}

// profilePhotoBuildOwnershipIndex applies the ownership backstop migration on
// its own connection and reports the result on a channel, so the test can
// observe the build while it is still running.
func profilePhotoBuildOwnershipIndex(ctx context.Context, build *pgx.Conn, body string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := build.Exec(ctx, body)
		done <- err
	}()
	return done
}

// profilePhotoWaitWhileBuilding blocks until the concurrent build is visible in
// pg_stat_activity, so the assertions that follow describe a running build
// rather than a race with a finished one.
func profilePhotoWaitWhileBuilding(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var building int64
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND query LIKE '%CREATE UNIQUE INDEX CONCURRENTLY files_id_uploader_id_key%'
			  AND state <> 'idle'
		`).Scan(&building); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if building > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("concurrent build never became observable in pg_stat_activity")
		}
		if _, err := conn.Exec(ctx, `SELECT pg_sleep($1)`, 100*time.Millisecond); err != nil {
			t.Fatalf("wait for build: %v", err)
		}
	}
}

func profilePhotoReadMigration(tb testing.TB, name string) string {
	tb.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
	if err != nil {
		tb.Fatalf("read migration %s: %v", name, err)
	}
	return string(body)
}

// profilePhotoStripComments returns the executable part of a SQL chunk, so the
// migration-shape test counts statements rather than comment lines.
func profilePhotoStripComments(chunk string) string {
	var kept []string
	for line := range strings.SplitSeq(chunk, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, " ")
}

func profilePhotoExplain(t *testing.T, ctx context.Context, conn *pgx.Conn, query string, args ...any) string {
	t.Helper()
	rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		fmt.Fprintln(&plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return plan.String()
}

// profilePhotoViolation reports whether err is the expected Postgres constraint
// failure. Naming the constraint, not just the SQLSTATE, is what keeps these
// assertions about the guarantee they claim to test.
func profilePhotoViolation(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
}
