package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// The gallery download gate's own tests. What is pinned here is what the lane
// above it depends on and cannot check for itself: which facts the one
// statement reads, that it reads the committed state (so a committed delete or
// block is in the next call's answer), that every rejection is the one error the
// client sees, and that the gate neither takes nor waits for the messaging
// lane's per-owner advisory key.
//
// Nothing writes user_photos in the server yet — the upload slice is separate —
// so the rows the gate reads are written here, in the shape a writer will write
// them.

// galleryGateQuota is room for every file these tests assemble.
const galleryGateQuota = int64(1) << 30

// galleryGateFixture is one owner, one viewer, and a raw connection to the same
// database the store's pool runs on: the raw connection is how a test writes a
// gallery row and how it holds a transaction open across a gate call.
type galleryGateFixture struct {
	s      *store.Store
	dsn    string
	conn   *pgx.Conn
	blobs  blob.Store
	owner  int64
	viewer int64
}

func newGalleryGateFixture(t *testing.T) *galleryGateFixture {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := testBlobs(t)
	s := openStore(t, dsn)
	owner, err := s.CreateUser(ctx, "+155594500001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	viewer, err := s.CreateUser(ctx, "+155594500002")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close observer: %v", err)
		}
	})
	return &galleryGateFixture{s: s, dsn: dsn, conn: conn, blobs: blobs, owner: owner.ID, viewer: viewer.ID}
}

// storedPhoto assembles a validated photo owned by ownerID and returns the row.
func (f *galleryGateFixture) storedPhoto(t *testing.T, ownerID int64) store.File {
	t.Helper()
	file, err := f.s.AllocateAndCompletePhotoFile(context.Background(), ownerID, 10, "image/jpeg",
		"photo.jpg", galleryGateQuota, func(file store.File) (store.PhotoDimensions, error) {
			_, err := f.blobs.Put(context.Background(), blob.Key(file.ID), bytes.NewReader([]byte("jpeg bytes")))
			return store.PhotoDimensions{Width: 640, Height: 480}, err
		})
	if err != nil {
		t.Fatalf("assemble photo: %v", err)
	}
	return file
}

// storedDocument assembles a stored document owned by ownerID: a file whose
// bytes exist and whose media kind is not a photo.
func (f *galleryGateFixture) storedDocument(t *testing.T, ownerID int64) store.File {
	t.Helper()
	file, err := f.s.AllocateAndCompleteFile(context.Background(), ownerID, 10, "text/plain",
		"file.txt", galleryGateQuota, nil, func(file store.File) error {
			_, err := f.blobs.Put(context.Background(), blob.Key(file.ID), bytes.NewReader([]byte("txt bytes")))
			return err
		})
	if err != nil {
		t.Fatalf("assemble document: %v", err)
	}
	return file
}

// pendingFile allocates a row without publishing bytes: the state a crashed
// assembly leaves behind.
func (f *galleryGateFixture) pendingFile(t *testing.T, ownerID int64) store.File {
	t.Helper()
	file, err := f.s.AllocateFile(context.Background(), ownerID, 10, "image/jpeg", "photo.jpg", galleryGateQuota)
	if err != nil {
		t.Fatalf("allocate file: %v", err)
	}
	return file
}

// addEntry writes the (owner, file) gallery row. client_file_id is the upload
// dedup key and is unique per owner, so the file id doubles as it here.
func (f *galleryGateFixture) addEntry(t *testing.T, ownerID, fileID int64) {
	t.Helper()
	if _, err := f.conn.Exec(context.Background(),
		`INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $2)`,
		ownerID, fileID); err != nil {
		t.Fatalf("add gallery entry (owner=%d file=%d): %v", ownerID, fileID, err)
	}
}

// deleteEntry removes it, the way a gallery delete will: the row is live and
// hard-deleted, not flagged.
func (f *galleryGateFixture) deleteEntry(t *testing.T, ownerID, fileID int64) {
	t.Helper()
	tag, err := f.conn.Exec(context.Background(),
		`DELETE FROM user_photos WHERE user_id = $1 AND file_id = $2`, ownerID, fileID)
	if err != nil {
		t.Fatalf("delete gallery entry (owner=%d file=%d): %v", ownerID, fileID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete gallery entry affected %d rows, want 1", tag.RowsAffected())
	}
}

// gate runs the lane's authorization for (ownerID, fileID) as viewerID.
func (f *galleryGateFixture) gate(t *testing.T, ownerID, fileID, viewerID int64) (store.File, error) {
	t.Helper()
	return f.s.ProfilePhotoForDownload(context.Background(), ownerID, fileID, viewerID)
}

func (f *galleryGateFixture) block(t *testing.T, blockerID, blockedID int64) {
	t.Helper()
	if _, err := f.s.BlockUser(context.Background(), blockerID, blockedID); err != nil {
		t.Fatalf("block %d -> %d: %v", blockerID, blockedID, err)
	}
}

// TestProfilePhotoGateAdmitsLiveOwnedStoredPhoto is the one case that must
// answer with a file: a live gallery entry naming a stored photo its owner
// uploaded, read by a viewer the owner has not blocked. The owner reading their
// own photo is the same statement, so it is asserted here too.
func TestProfilePhotoGateAdmitsLiveOwnedStoredPhoto(t *testing.T) {
	t.Parallel()
	f := newGalleryGateFixture(t)
	photo := f.storedPhoto(t, f.owner)
	f.addEntry(t, f.owner, photo.ID)

	file, err := f.gate(t, f.owner, photo.ID, f.viewer)
	if err != nil {
		t.Fatalf("gate for an authorized viewer: %v", err)
	}
	if file.ID != photo.ID {
		t.Fatalf("gate returned file %d, want %d", file.ID, photo.ID)
	}
	if file.Kind != store.FileKindPhoto || !file.Stored {
		t.Fatalf("gate returned kind=%q stored=%v, want a stored photo", file.Kind, file.Stored)
	}
	if file.UploaderID != f.owner {
		t.Fatalf("gate returned uploader %d, want the entry's owner %d", file.UploaderID, f.owner)
	}

	if _, err := f.gate(t, f.owner, photo.ID, f.owner); err != nil {
		t.Fatalf("gate for the owner reading their own photo: %v", err)
	}
}

// TestProfilePhotoGateRejectionsAreOneError is the negative matrix. Every row
// must fail as store.ErrFileNotFound and nothing else: the file id space is
// dense, so a download that answers "no such file" separately from "not your
// gallery entry" is an enumeration oracle over every account's gallery.
func TestProfilePhotoGateRejectionsAreOneError(t *testing.T) {
	t.Parallel()
	f := newGalleryGateFixture(t)

	// A photo in this owner's gallery, for the cross-owner cases.
	theirs := f.storedPhoto(t, f.viewer)
	f.addEntry(t, f.viewer, theirs.ID)

	cases := map[string]struct {
		owner, file, viewer int64
	}{
		"no gallery entry": {owner: f.owner, file: f.storedPhoto(t, f.owner).ID, viewer: f.viewer},
		"entry deleted": {
			owner: f.owner,
			file: func() int64 {
				p := f.storedPhoto(t, f.owner)
				f.addEntry(t, f.owner, p.ID)
				f.deleteEntry(t, f.owner, p.ID)
				return p.ID
			}(),
			viewer: f.viewer,
		},
		"file sits in another owner's gallery": {owner: f.owner, file: theirs.ID, viewer: f.viewer},
		"no such file":                         {owner: f.owner, file: 999_999_999, viewer: f.viewer},
		"no such owner":                        {owner: 999_999_998, file: 999_999_999, viewer: f.viewer},
		"bytes never published": {
			// An allocation that never completed: a row, no bytes. The gate's two
			// predicates answer it together — files_media_metadata_valid admits a
			// photo kind only alongside stored=true and validated dimensions, so a
			// row that is not published can never be a published photo.
			owner: f.owner,
			file: func() int64 {
				pending := f.pendingFile(t, f.owner)
				f.addEntry(t, f.owner, pending.ID)
				return pending.ID
			}(),
			viewer: f.viewer,
		},
		"stored file is not a photo": {
			owner: f.owner,
			file: func() int64 {
				doc := f.storedDocument(t, f.owner)
				f.addEntry(t, f.owner, doc.ID)
				return doc.ID
			}(),
			viewer: f.viewer,
		},
		"target blocked the viewer": {
			owner: f.owner,
			file: func() int64 {
				p := f.storedPhoto(t, f.owner)
				f.addEntry(t, f.owner, p.ID)
				f.block(t, f.owner, f.viewer)
				return p.ID
			}(),
			viewer: f.viewer,
		},
	}

	for name, c := range cases {
		_, err := f.gate(t, c.owner, c.file, c.viewer)
		if !errors.Is(err, store.ErrFileNotFound) {
			t.Errorf("%s: got %v, want store.ErrFileNotFound", name, err)
		}
	}
}

// TestProfilePhotoGateBlockIsDirected pins that the block predicate reads one
// edge. The photo owner refusing a viewer ends avatar delivery; the viewer
// refusing the owner is that viewer's own choice about their own inbox, and it
// says nothing about who may read an avatar.
func TestProfilePhotoGateBlockIsDirected(t *testing.T) {
	t.Parallel()
	f := newGalleryGateFixture(t)
	photo := f.storedPhoto(t, f.owner)
	f.addEntry(t, f.owner, photo.ID)

	f.block(t, f.viewer, f.owner)
	if _, err := f.gate(t, f.owner, photo.ID, f.viewer); err != nil {
		t.Fatalf("gate with the viewer blocking the owner: %v, want the read admitted", err)
	}
}

// TestProfilePhotoGateAnswersCommittedStateOnly is the revocation contract, and
// it is an isolation property of the one statement: an in-flight write is
// invisible, and the statement that starts after the commit sees it. This is
// what lets the lane drop its authorization on the floor — there is no
// transaction to keep the answer open, and no lock to hold it.
func TestProfilePhotoGateAnswersCommittedStateOnly(t *testing.T) {
	t.Parallel()
	f := newGalleryGateFixture(t)
	ctx := context.Background()

	// One photo for the delete and one for the block: committing the first delete
	// retires the entry that the block case needs live.
	photo := f.storedPhoto(t, f.owner)
	f.addEntry(t, f.owner, photo.ID)

	tx, err := f.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_photos WHERE user_id = $1 AND file_id = $2`, f.owner, photo.ID); err != nil {
		t.Fatalf("delete in transaction: %v", err)
	}
	if _, err := f.gate(t, f.owner, photo.ID, f.viewer); err != nil {
		t.Fatalf("gate with the delete uncommitted: %v, want the read admitted", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
	if _, err := f.gate(t, f.owner, photo.ID, f.viewer); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("gate after the delete committed: %v, want store.ErrFileNotFound", err)
	}

	// The same ordering for a block: committing it is what revokes access, and
	// the statement that starts afterwards is the call that is denied.
	blocked := f.storedPhoto(t, f.owner)
	f.addEntry(t, f.owner, blocked.ID)
	tx, err = f.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin block tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO blocked_users (blocker_id, blocked_id) VALUES ($1, $2)`, f.owner, f.viewer); err != nil {
		t.Fatalf("insert block in transaction: %v", err)
	}
	if _, err := f.gate(t, f.owner, blocked.ID, f.viewer); err != nil {
		t.Fatalf("gate with the block uncommitted: %v, want the read admitted", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit block: %v", err)
	}
	if _, err := f.gate(t, f.owner, blocked.ID, f.viewer); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("gate after the block committed: %v, want store.ErrFileNotFound", err)
	}
}

// TestProfilePhotoGateDoesNotWaitOnOwnerMessagingLock is the property the
// messaging lane's latency depends on. SendMessage serializes on the per-owner
// advisory key, so a download gate that took that key — or waited on it — would
// put every avatar read in front of the owner's own sends. Here the owner's key
// is held open by another session while the gate runs: the gate must answer
// at once, and must leave no advisory lock of its own behind.
func TestProfilePhotoGateDoesNotWaitOnOwnerMessagingLock(t *testing.T) {
	t.Parallel()
	f := newGalleryGateFixture(t)
	ctx := context.Background()

	photo := f.storedPhoto(t, f.owner)
	f.addEntry(t, f.owner, photo.ID)

	tx, err := f.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, f.owner); err != nil {
		t.Fatalf("take the owner's messaging key: %v", err)
	}
	if n := galleryAdvisoryHolders(t, ctx, f.conn, f.owner); n == 0 {
		t.Fatal("the owner's advisory key is not held: the fixture proves nothing")
	}

	// A gate that waited on that key would hit this deadline instead of answering.
	callerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := f.s.ProfilePhotoForDownload(callerCtx, f.owner, photo.ID, f.viewer); err != nil {
		t.Fatalf("gate while the owner's messaging key is held: %v", err)
	}
	if n := galleryAdvisoryHolders(t, ctx, f.conn, f.owner); n != 1 {
		t.Fatalf("advisory holders on the owner's key = %d, want 1: the gate took a lock", n)
	}
	if n := galleryAllAdvisoryLocks(t, ctx, f.conn); n != 1 {
		t.Fatalf("granted advisory locks in this database = %d, want only the fixture's", n)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// galleryAdvisoryHolders counts the sessions holding the messaging lane's
// single-key advisory lock for ownerID, in this test's database. pg_locks is
// cluster-wide and every cloned database numbers its users from the same small
// sequence, so an unscoped count is other tests' locks.
//
// Postgres splits a single big-int advisory key into two unsigned 32-bit halves
// with objsubid 1, a key space separate from the two-argument form (objsubid 2);
// the messaging lane uses the single-key form, and that is the form pinned here.
// User ids come from a positive sequence, so reassembling the halves yields the
// key that was taken.
func galleryAdvisoryHolders(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.objsubid = 1 AND l.granted
		  AND a.datname = current_database()
		  AND (l.classid::bigint << 32) | l.objid::bigint = $1`,
		ownerID).Scan(&n); err != nil {
		t.Fatalf("count advisory holders: %v", err)
	}
	return n
}

// galleryAllAdvisoryLocks counts every granted advisory lock in this database.
// pg_locks is cluster-wide, so the database is the scope a test owns.
func galleryAllAdvisoryLocks(t *testing.T, ctx context.Context, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND a.datname = current_database()`,
	).Scan(&n); err != nil {
		t.Fatalf("count advisory locks: %v", err)
	}
	return n
}
