package store_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// The gallery lane's synthetic upload tests. Nothing is registered, so every
// call here is a direct store call: that is the only way this path can be
// reached until its RPC slice exists.
//
// The blob backend is a counting wrapper, so "one Put", "no new Put" and "a
// failed Put wrote no object" are counts and reads, not inferences.

const (
	profilePhotoSize    = 200 // two 100-byte parts
	profilePerFileCap   = 8 << 20
	profileGalleryLimit = 4
)

// profileBlobs counts the object writes and removals that reach the blob
// backend, split by lane: part keys are upload parts, anything else is an
// assembled file.
type profileBlobs struct {
	blob.Store

	mu      sync.Mutex
	puts    []string
	removes []string
}

func newProfileBlobs(t *testing.T) *profileBlobs {
	t.Helper()
	return &profileBlobs{Store: testBlobs(t)}
}

func (b *profileBlobs) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	b.mu.Lock()
	b.puts = append(b.puts, key)
	b.mu.Unlock()
	return b.Store.Put(ctx, key, r)
}

func (b *profileBlobs) Remove(ctx context.Context, key string) error {
	b.mu.Lock()
	b.removes = append(b.removes, key)
	b.mu.Unlock()
	return b.Store.Remove(ctx, key)
}

// filePuts is the assembled-file writes: the gallery lane's Put, and the
// messaging lane's, which no test here exercises.
func (b *profileBlobs) filePuts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, k := range b.puts {
		if !strings.HasPrefix(k, blob.PartsPrefix) {
			out = append(out, k)
		}
	}
	return out
}

func profileStoreOn(t *testing.T, b *profileBlobs, dsn string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), dsn, pgtest.EncKey(), store.WithBlobStore(b))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

// profilePartsT writes payloads as one client upload's parts, the way
// upload.saveFilePart does.
func profilePartsT(t *testing.T, s *store.Store, owner, clientFileID int64, payloads ...[]byte) {
	t.Helper()
	for i, payload := range payloads {
		if err := s.SaveUploadPart(context.Background(), owner, clientFileID, i, payload, profilePerFileCap); err != nil {
			t.Fatalf("save part %d: %v", i, err)
		}
	}
}

// profileWriteBlob is the synthetic Put: it reads the parts the store measured,
// writes them under the file's own key, and reports the dimensions it claims to
// have validated. It is the shape the photo RPC will hand to ProfileUpload when
// that slice exists.
func profileWriteBlob(s *store.Store, a store.ProfileAssembly) (store.PhotoDimensions, error) {
	buf := bytes.NewBuffer(make([]byte, 0, a.Total))
	for _, ref := range a.Parts {
		payload, err := s.ReadUploadPart(context.Background(), ref)
		if err != nil {
			return store.PhotoDimensions{}, err
		}
		buf.Write(payload)
	}
	if int64(buf.Len()) != a.Total {
		return store.PhotoDimensions{}, fmt.Errorf("read %d bytes, expected %d", buf.Len(), a.Total)
	}
	if _, err := store.BlobsOf(s).Put(context.Background(), blob.Key(a.File.ID), bytes.NewReader(buf.Bytes())); err != nil {
		return store.PhotoDimensions{}, err
	}
	return store.PhotoDimensions{Width: 20, Height: 10}, nil
}

func profileRequest(ownerID, clientFileID int64, parts, galleryCap int, quota int64,
	assemble func(store.ProfileAssembly) (store.PhotoDimensions, error),
) store.ProfileUploadRequest {
	return store.ProfileUploadRequest{
		OwnerID:      ownerID,
		ClientFileID: clientFileID,
		Parts:        parts,
		MimeType:     "image/jpeg",
		FileName:     "profile.jpg",
		MaxFileBytes: 1 << 20,
		MaxUserBytes: quota,
		GalleryCap:   galleryCap,
		Assemble:     assemble,
	}
}

// profileDB is the state the acceptance criteria are counted on: the file rows
// and their summed size (the lifetime charge), the gallery entries, the receipt,
// and the current selection.
type profileDB struct {
	fileRows     int64
	storedRows   int64
	chargedBytes int64
	galleryRows  int64
	receiptRows  int64
	receiptState int16
	receiptFound bool
	receiptFile  sql.NullInt64
	receiptSize  int64
	receiptParts int64
	currentFile  sql.NullInt64
	revision     int64
	receiptTime  time.Time
}

func profileSnapshot(t *testing.T, ctx context.Context, dsn string, owner, clientFileID int64) profileDB {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	var s profileDB
	row := conn.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM files WHERE uploader_id = $1),
		  (SELECT count(*) FROM files WHERE uploader_id = $1 AND stored),
		  (SELECT coalesce(sum(size), 0) FROM files WHERE uploader_id = $1),
		  (SELECT count(*) FROM user_photos WHERE user_id = $1),
		  (SELECT count(*) FROM profile_upload_receipt WHERE user_id = $1)`, owner)
	if err := row.Scan(&s.fileRows, &s.storedRows, &s.chargedBytes, &s.galleryRows, &s.receiptRows); err != nil {
		t.Fatalf("scan counts: %v", err)
	}
	row = conn.QueryRow(ctx, `
		SELECT state, file_id, request_size, part_count, updated_at
		FROM profile_upload_receipt WHERE user_id = $1 AND client_file_id = $2`, owner, clientFileID)
	err = row.Scan(&s.receiptState, &s.receiptFile, &s.receiptSize, &s.receiptParts, &s.receiptTime)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s
	case err != nil:
		t.Fatalf("scan receipt: %v", err)
	}
	s.receiptFound = true
	row = conn.QueryRow(ctx, `SELECT current_file_id, mutation_revision FROM profile_photo_state WHERE user_id = $1`, owner)
	if err := row.Scan(&s.currentFile, &s.revision); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("scan state: %v", err)
	}
	return s
}

// The objsubid half of pg_locks' advisory rows, as Postgres writes them: a
// single big-int key is objsubid 1 with the value split across classid and objid,
// and the two-argument form is objsubid 2 with each argument in its own column.
// The two forms are separate key spaces, which is the whole of why the profile
// domain cannot collide with the messaging owner keys.
const (
	profileSingleKeySpace int16 = 1
	profileTwoKeySpace    int16 = 2
	// profileNegativeKeyClass is objsubid 1's classid for a negative
	// big-int key: the top half of the int64, which the assembly claim's
	// -fileID keys always are. pg_locks reports classid as an unsigned oid, so
	// -1 is 4294967295.
	profileNegativeKeyClass int64 = 1<<32 - 1
)

// profileAdvisory is one advisory-lock row as an observer reads it. classid and
// objid are unsigned, so both are ints widened to 64 bits.
type profileAdvisory struct {
	Class   int64
	Obj     int64
	Sub     int16
	Granted bool
}

func profileAdvisoryLocks(t *testing.T, ctx context.Context, conn *pgx.Conn, pid int) []profileAdvisory {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT classid::bigint, objid::bigint, objsubid, granted
		FROM pg_locks
		WHERE pid = $1 AND locktype = 'advisory'`, pid)
	if err != nil {
		t.Fatalf("read advisory locks: %v", err)
	}
	defer rows.Close()
	var out []profileAdvisory
	for rows.Next() {
		var a profileAdvisory
		if err := rows.Scan(&a.Class, &a.Obj, &a.Sub, &a.Granted); err != nil {
			t.Fatalf("scan advisory lock: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("advisory locks: %v", err)
	}
	return out
}

// profileSession is the uploading session as pg_stat_activity
// sees it: which statement it is running, and what it is waiting on. A session
// blocked on a lock reports wait_event_type 'Lock' and keeps its running
// statement visible, which is how the completion's lock order is observed: the
// statement it is blocked *at* is the receipt lock, not the file interlock.
type profileSession struct {
	Pid       int
	State     string
	WaitType  string
	WaitEvent string
	Query     string
}

// profileUploadSession returns the session that holds the profile-domain hold,
// with its current statement and wait state.
func profileUploadSession(t *testing.T, ctx context.Context, conn *pgx.Conn, owner int64) profileSession {
	t.Helper()
	var s profileSession
	err := conn.QueryRow(ctx, `
		SELECT a.pid, a.state, coalesce(a.wait_event_type, ''), coalesce(a.wait_event, ''), a.query
		FROM pg_stat_activity a
		JOIN pg_locks l
		  ON l.pid = a.pid AND l.locktype = 'advisory' AND l.objsubid = 2
		 AND l.classid::bigint = $1 AND l.objid::bigint = $2 AND l.granted
		WHERE a.pid <> pg_backend_pid()`,
		int64(store.ProfileLockDomain), owner).
		Scan(&s.Pid, &s.State, &s.WaitType, &s.WaitEvent, &s.Query)
	if errors.Is(err, pgx.ErrNoRows) {
		return profileSession{}
	}
	if err != nil {
		t.Fatalf("read the uploading session: %v", err)
	}
	return s
}

// profileHoldsRelation reports whether a session holds any
// lock on a table. The completion takes the receipt and state row locks and the
// files reference lock, and each takes a table-level lock with it, so "no lock
// on files yet" is "not at the file interlock yet".
func profileHoldsRelation(t *testing.T, ctx context.Context, conn *pgx.Conn, pid int, rel string) bool {
	t.Helper()
	return profileCount(t, ctx, conn, `
		SELECT count(*) FROM pg_locks
		WHERE pid = $1 AND locktype = 'relation' AND relation = $2::regclass`, pid, rel) > 0
}

// profileLockLines dumps the lock rows of the given sessions, for a failure that
// needs to say what the lock table actually showed.
func profileLockLines(t *testing.T, ctx context.Context, conn *pgx.Conn, pids []int) string {
	t.Helper()
	rows, err := conn.Query(ctx, `
		SELECT l.pid, l.locktype, coalesce(l.relation::regclass::text, l.classid::text, 'n/a'), l.mode, l.granted
		FROM pg_locks l
		WHERE l.pid = ANY($1::int[])
		ORDER BY l.pid, l.locktype, 2`, pids)
	if err != nil {
		t.Fatalf("dump locks: %v", err)
	}
	defer rows.Close()
	var out strings.Builder
	for rows.Next() {
		var pid int
		var locktype, rel, mode string
		var granted bool
		if err := rows.Scan(&pid, &locktype, &rel, &mode, &granted); err != nil {
			t.Fatalf("scan lock row: %v", err)
		}
		fmt.Fprintf(&out, "[pid=%d %s %s %s granted=%v]", pid, locktype, rel, mode, granted)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("lock rows: %v", err)
	}
	return out.String()
}

// profileBlockedOnRelation reports whether a session is queued on a
// table-level lock: the way a control proves the upload's hold is live.
func profileBlockedOnRelation(t *testing.T, ctx context.Context, conn *pgx.Conn, pid int, rel string) bool {
	t.Helper()
	return profileCount(t, ctx, conn, `
		SELECT count(*) FROM pg_locks
		WHERE pid = $1 AND locktype = 'relation' AND relation = $2::regclass AND NOT granted`,
		pid, rel) > 0
}

// profileUploadPID finds the session holding the profile-domain hold.
func profileUploadPID(t *testing.T, ctx context.Context, conn *pgx.Conn, owner int64) int {
	t.Helper()
	var pid int
	if err := conn.QueryRow(ctx, `
		SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2
		  AND classid::bigint = $1 AND objid::bigint = $2 AND granted`,
		int64(store.ProfileLockDomain), owner).Scan(&pid); err != nil {
		t.Fatalf("find the profile-domain holder: %v", err)
	}
	return pid
}

// profileCount runs a count query on an observer connection.
func profileCount(t *testing.T, ctx context.Context, conn *pgx.Conn, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// profileWait polls a condition so the observer asserts on a state the run
// produced rather than on a sleep.
func profileWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// profileFixture is one owner, one counting blob backend, and the synthetic
// upload path in front of them.
type profileFixture struct {
	t     *testing.T
	dsn   string
	s     *store.Store
	blobs *profileBlobs
	owner int64
}

func newProfileFixture(t *testing.T) *profileFixture {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := newProfileBlobs(t)
	s := profileStoreOn(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15559300101")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	return &profileFixture{t: t, dsn: dsn, s: s, blobs: blobs, owner: owner.ID}
}

// upload runs one synthetic gallery upload with the default Put.
func (f *profileFixture) upload(clientFileID int64, parts, galleryCap int, quota int64) (store.ProfileUploadResult, error) {
	f.t.Helper()
	return f.s.ProfileUpload(context.Background(), profileRequest(f.owner, clientFileID, parts, galleryCap, quota,
		func(a store.ProfileAssembly) (store.PhotoDimensions, error) { return profileWriteBlob(f.s, a) }))
}

// blobHas reports whether the assembled file's object is in the blob store,
// which is the only place a failed Put can leave evidence.
func (f *profileFixture) blobHas(fileID int64) bool {
	f.t.Helper()
	_, err := f.blobs.ReadAt(context.Background(), blob.Key(fileID), 0, 1)
	return err == nil
}

// TestProfileUploadConcurrentIdenticalUploadsOneCharge is the dedup criterion at
// the exact edge: quota is exactly one photo, two processes run the same
// upload at the same time, and the answer is one stored file, one gallery entry,
// one lifetime charge, and the same identity in both responses.
func TestProfileUploadConcurrentIdenticalUploadsOneCharge(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()

	// Two process instances: separate pools, separate sessions, one database and
	// one blob backend, so the Put count is the whole system's.
	second := profileStoreOn(t, f.blobs, f.dsn)
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))
	profilePartsT(t, second, f.owner, 7, part('a', 100), part('b', 100))

	const quota = profilePhotoSize // exactly one photo
	type outcome struct {
		res store.ProfileUploadResult
		err error
	}
	outcomes := make(chan outcome, 2)
	for _, s := range []*store.Store{f.s, second} {
		go func(s *store.Store) {
			res, err := s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, quota,
				func(a store.ProfileAssembly) (store.PhotoDimensions, error) { return profileWriteBlob(s, a) }))
			outcomes <- outcome{res: res, err: err}
		}(s)
	}

	var results []store.ProfileUploadResult
	deadline := time.After(30 * time.Second)
	for range 2 {
		select {
		case o := <-outcomes:
			if o.err != nil {
				t.Fatalf("concurrent identical upload: %v", o.err)
			}
			results = append(results, o.res)
		case <-deadline:
			t.Fatal("concurrent identical uploads did not both return")
		}
	}
	first, duplicate := results[0], results[1]
	if duplicate.File.ID < first.File.ID {
		first, duplicate = duplicate, first
	}
	if first.File.ID != duplicate.File.ID {
		t.Fatalf("one key returned file ids %d and %d", first.File.ID, duplicate.File.ID)
	}
	if first.File.AccessHash != duplicate.File.AccessHash {
		t.Fatal("one key returned two access hashes")
	}
	if !duplicate.Replayed {
		t.Fatal("the losing upload was not answered from the receipt")
	}
	if first.File.Width != 20 || first.File.Height != 10 || !first.File.Stored {
		t.Fatalf("the winner reports %+v, want the validated stored photo", first.File)
	}

	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.fileRows != 1 {
		t.Fatalf("files rows = %d, want 1", snap.fileRows)
	}
	if snap.storedRows != 1 {
		t.Fatalf("stored files rows = %d, want 1", snap.storedRows)
	}
	if snap.chargedBytes != profilePhotoSize {
		t.Fatalf("lifetime charge = %d bytes, want exactly one photo (%d)", snap.chargedBytes, profilePhotoSize)
	}
	if snap.galleryRows != 1 {
		t.Fatalf("gallery entries = %d, want 1", snap.galleryRows)
	}
	if snap.receiptRows != 1 || snap.receiptState != 1 {
		t.Fatalf("receipts = %d state %d, want 1 complete", snap.receiptRows, snap.receiptState)
	}
	if !snap.currentFile.Valid || snap.currentFile.Int64 != first.File.ID {
		t.Fatalf("current selection = %v, want the uploaded file %d", snap.currentFile, first.File.ID)
	}
	if snap.revision != 1 {
		t.Fatalf("mutation revision = %d, want 1", snap.revision)
	}
	if puts := f.blobs.filePuts(); len(puts) != 1 {
		t.Fatalf("blob Puts = %d (%v), want 1", len(puts), puts)
	}
}

// TestProfileUploadCompletedRetryAfterPartsCleanupMakesNoChange is the retry
// identity criterion: the parts are gone (assembly deletes them) and the retry is
// answered from the receipt with no Put, no allocation and no selection change.
// If the receipt lookup did not precede the part reads, this request would die on
// the vanished parts exactly as the pre-receipt path does.
func TestProfileUploadCompletedRetryAfterPartsCleanupMakesNoChange(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	first, err := f.upload(7, 2, 0, 1<<30)
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	before := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	putsBefore := len(f.blobs.filePuts())

	if _, err := f.s.DeleteUploadParts(ctx, f.owner, 7); err != nil {
		t.Fatalf("delete parts: %v", err)
	}
	if parts, _, _, err := f.s.UploadPartsSummary(ctx, f.owner, 7); err != nil || parts != 0 {
		t.Fatalf("parts after cleanup = %d (err %v), want 0", parts, err)
	}

	retry, err := f.upload(7, 2, 1, 1<<30)
	if err != nil {
		t.Fatalf("retry after parts cleanup: %v", err)
	}
	if !retry.Replayed {
		t.Fatal("the retry is not marked as answered from the receipt")
	}
	if retry.File.ID != first.File.ID || retry.File.AccessHash != first.File.AccessHash {
		t.Fatalf("retry identity %d/%d differs from the stored %d/%d",
			retry.File.ID, retry.File.AccessHash, first.File.ID, first.File.AccessHash)
	}
	if !retry.File.Stored || retry.File.Kind != store.FileKindPhoto {
		t.Fatalf("retry reports a %s stored=%v file", retry.File.Kind, retry.File.Stored)
	}
	if retry.MutationRevision != before.revision {
		t.Fatalf("retry reported revision %d, stored %d", retry.MutationRevision, before.revision)
	}

	after := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if puts := len(f.blobs.filePuts()); puts != putsBefore {
		t.Fatalf("blob Puts went from %d to %d; a completed key must not Put", putsBefore, puts)
	}
	if after.fileRows != before.fileRows {
		t.Fatalf("files rows went from %d to %d; a completed key must not allocate", before.fileRows, after.fileRows)
	}
	if after.chargedBytes != before.chargedBytes {
		t.Fatalf("lifetime charge went from %d to %d bytes", before.chargedBytes, after.chargedBytes)
	}
	if after.galleryRows != before.galleryRows {
		t.Fatalf("gallery entries went from %d to %d", before.galleryRows, after.galleryRows)
	}
	if after.currentFile.Int64 != before.currentFile.Int64 || after.revision != before.revision {
		t.Fatalf("selection changed: current %v→%v revision %d→%d",
			before.currentFile, after.currentFile, before.revision, after.revision)
	}
	if !after.receiptTime.Equal(before.receiptTime) {
		t.Fatalf("receipt updated_at moved %v→%v", before.receiptTime, after.receiptTime)
	}
	if after.receiptState != 1 {
		t.Fatalf("receipt state = %d, want complete", after.receiptState)
	}
}

// TestProfileUploadRejectsChangedContentUnderSameKey pins the fingerprint rule
// and its uniformity: a key that bought one request answers every different
// request under that key with one error, whether the change is the declared part
// count, the measured size, or the payload digest.
func TestProfileUploadRejectsChangedContentUnderSameKey(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))
	if _, err := f.upload(7, 2, 0, 1<<30); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	// A different payload under the same key, with the parts rewritten.
	if _, err := f.s.DeleteUploadParts(ctx, f.owner, 7); err != nil {
		t.Fatalf("clear parts: %v", err)
	}
	profilePartsT(t, f.s, f.owner, 7, part('c', 100), part('d', 100))
	_, changedPayload := f.upload(7, 2, 0, 1<<30)

	// A different declared part count for the same key.
	_, changedCount := f.upload(7, 3, 0, 1<<30)

	// A different measured size at the same part count.
	if _, err := f.s.DeleteUploadParts(ctx, f.owner, 7); err != nil {
		t.Fatalf("clear parts: %v", err)
	}
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 120))
	_, changedSize := f.upload(7, 2, 0, 1<<30)

	for name, err := range map[string]error{
		"changed payload": changedPayload,
		"changed count":   changedCount,
		"changed size":    changedSize,
	} {
		if !errors.Is(err, store.ErrProfileUploadConflict) {
			t.Errorf("%s: want ErrProfileUploadConflict, got %v", name, err)
		}
	}

	// None of the three rejections wrote anything.
	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.fileRows != 1 || snap.galleryRows != 1 {
		t.Fatalf("the rejections changed the rows: files=%d gallery=%d", snap.fileRows, snap.galleryRows)
	}
	if snap.chargedBytes != profilePhotoSize {
		t.Fatalf("the rejections moved the charge to %d bytes", snap.chargedBytes)
	}
	if puts := len(f.blobs.filePuts()); puts != 1 {
		t.Fatalf("the rejections produced %d Puts, want 1", puts)
	}
	if snap.receiptState != 1 {
		t.Fatalf("a rejection moved the receipt to state %d", snap.receiptState)
	}
}

// TestProfileUploadPausedPutDoesNotHoldMessagingOwnerLock is the accepted
// resource-hold criterion. With the Put paused inside the completion, inbound DMs
// and group sends to that owner commit, and the observer reads the locks the
// uploading session actually holds: the profile-domain hold (the two-argument
// form, objsubid 1) and the assembly claim, and no bare messaging owner key.
func TestProfileUploadPausedPutDoesNotHoldMessagingOwnerLock(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	sender, err := f.s.CreateUser(ctx, "+15559300102")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	chat, err := f.s.CreateChat(ctx, sender.ID, "observed", []int64{f.owner})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	// A separate process, so the messaging sends are ordinary traffic against a
	// database whose gallery lane is mid-Put.
	peer := profileStoreOn(t, newProfileBlobs(t), f.dsn)

	ready := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, 1<<30,
			func(a store.ProfileAssembly) (store.PhotoDimensions, error) {
				once.Do(func() {
					close(ready)
					<-release
				})
				return profileWriteBlob(f.s, a)
			}))
		done <- err
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the upload never reached its paused Put")
	}

	observer, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect observer: %v", err)
	}
	defer func() { _ = observer.Close(ctx) }() //nolint:errcheck // best-effort close

	pid := profileUploadPID(t, ctx, observer, f.owner)
	locks := profileAdvisoryLocks(t, ctx, observer, pid)

	var profileDomain, bareOwnerKey bool
	claims := 0
	for _, l := range locks {
		if !l.Granted {
			continue
		}
		switch {
		case l.Sub == profileTwoKeySpace && l.Class == int64(store.ProfileLockDomain) && l.Obj == f.owner:
			profileDomain = true
		case l.Sub == profileSingleKeySpace && l.Class == 0 && l.Obj == f.owner:
			bareOwnerKey = true
		case l.Sub == profileSingleKeySpace && l.Class == profileNegativeKeyClass:
			claims++
		}
	}
	if !profileDomain {
		t.Fatalf("the uploading session holds no profile-domain advisory lock: %+v", locks)
	}
	if bareOwnerKey {
		t.Fatalf("the uploading session holds a bare messaging owner advisory lock across Put: %+v", locks)
	}
	if claims != 1 {
		t.Fatalf("the uploading session holds %d assembly claims, want 1: %+v", claims, locks)
	}

	// Messaging to the same owner commits while the Put is paused.
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sent := make(chan error, 2)
	go func() {
		_, _, _, _, err := peer.SendMessage(sendCtx, sender.ID, f.owner, "inbound dm", 9101, 0, 0)
		sent <- err
	}()
	go func() {
		_, _, _, err := peer.SendChatMessage(sendCtx, store.FanOut{
			ChatID: chat.ID, FromID: sender.ID, Text: "inbound group", RandomID: 9102,
		})
		sent <- err
	}()
	for range 2 {
		select {
		case err := <-sent:
			if err != nil {
				t.Fatalf("a messaging send to the owner failed while Put was paused: %v", err)
			}
		case <-sendCtx.Done():
			t.Fatal("a messaging send to the owner did not commit while Put was paused")
		}
	}

	// The sends are committed and visible, not merely unblocked.
	var messages int64
	if err := observer.QueryRow(ctx, `
		SELECT count(*) FROM messages
		WHERE owner_id = $1 AND message IN ('inbound dm', 'inbound group')`, f.owner).
		Scan(&messages); err != nil {
		t.Fatalf("count sends: %v", err)
	}
	if messages != 2 {
		t.Fatalf("committed sends visible to the owner = %d, want 2", messages)
	}

	releaseOnce()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("upload after release: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the upload did not finish after the Put was released")
	}
	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.storedRows != 1 || snap.galleryRows != 1 || snap.receiptState != 1 {
		t.Fatalf("the upload did not land: %+v", snap)
	}
}

// TestProfileUploadCompletionLockOrderIsReceiptStateThenFile observes the
// completion's lock order on a live system, in the two halves the criterion
// states.
//
// First half: with a control transaction holding the key's receipt row, the
// restart is seen stopped at the receipt lock (pg_stat_activity names the
// statement it is blocked at) with no lock of any kind on files, so the receipt
// and state locks precede the terminal file-reference interlock.
//
// Second half: with the Put paused, controls queue behind
// table-level holds on the receipt, the state row's table and the files table,
// and nothing on the gallery table, so the receipt/state holds and the file
// interlock are all live across the blob write, the gallery write comes after
// it, and no lock is taken after the file hold that a receipt/state/file
// writer would conflict with.
func TestProfileUploadCompletionLockOrderIsReceiptStateThenFile(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	failed := errors.New("blob put: crashed mid-write")
	if _, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, 1<<30,
		func(store.ProfileAssembly) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{}, failed
		})); !errors.Is(err, failed) {
		t.Fatalf("want the first assembly to fail, got %v", err)
	}
	pending := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if !pending.receiptFound || pending.receiptState != 0 {
		t.Fatalf("the first attempt left no pending receipt: %+v", pending)
	}

	observer, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect observer: %v", err)
	}
	defer func() { _ = observer.Close(ctx) }() //nolint:errcheck // best-effort close

	// A control transaction takes the receipt row, the lock the completion takes
	// first. It bounds its own wait so a broken ordering cannot wedge the run.
	control, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect control: %v", err)
	}
	defer func() { _ = control.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := control.Exec(ctx, `SET lock_timeout = '20s'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	if _, err := control.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("begin control: %v", err)
	}
	defer func() { _, _ = control.Exec(context.Background(), "ROLLBACK") }() //nolint:errcheck // best-effort
	if _, err := control.Exec(ctx, `
		SELECT user_id FROM profile_upload_receipt
		WHERE user_id = $1 AND client_file_id = 7 FOR UPDATE`, f.owner); err != nil {
		t.Fatalf("control receipt lock: %v", err)
	}

	putReached := make(chan int64, 1)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	done := make(chan error, 1)
	go func() {
		_, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, 1<<30,
			func(a store.ProfileAssembly) (store.PhotoDimensions, error) {
				putReached <- a.File.ID
				<-release
				return profileWriteBlob(f.s, a)
			}))
		done <- err
	}()

	// (1) Stopped at the receipt lock, with the file interlock not yet reached.
	var session profileSession
	profileWait(t, "the restart to stop at the receipt row lock", func() bool {
		session = profileUploadSession(t, ctx, observer, f.owner)
		return session.Pid != 0 && session.State == "active" && session.WaitType == "Lock" &&
			strings.Contains(session.Query, "profile_upload_receipt") &&
			strings.Contains(strings.ToUpper(session.Query), "FOR UPDATE")
	})
	if profileHoldsRelation(t, ctx, observer, session.Pid, "files") {
		t.Fatalf("the completion took a files lock at the receipt lock: %+v", session)
	}
	if profileHoldsRelation(t, ctx, observer, session.Pid, "user_photos") {
		t.Fatalf("the completion touched the gallery table at the receipt lock: %+v", session)
	}
	advisory := profileAdvisoryLocks(t, ctx, observer, session.Pid)
	var domain, bareOwner bool
	for _, l := range advisory {
		if l.Sub == profileTwoKeySpace && l.Class == int64(store.ProfileLockDomain) && l.Obj == f.owner {
			domain = true
		}
		if l.Sub == profileSingleKeySpace && l.Class == 0 && l.Obj == f.owner {
			bareOwner = true
		}
	}
	if !domain {
		t.Fatalf("the session stopped at the receipt lock is not the profile-domain holder: %+v", advisory)
	}
	if bareOwner {
		t.Fatalf("the completion holds a bare messaging owner key: %+v", advisory)
	}

	// Release the receipt row: the completion proceeds to the Put.
	if _, err := control.Exec(ctx, "COMMIT"); err != nil {
		t.Fatalf("commit control: %v", err)
	}
	select {
	case <-putReached:
	case <-time.After(20 * time.Second):
		t.Fatalf("the restart never reached its Put (blocked at %+v)", session)
	}

	// (2) Across the paused Put, the receipt, state and files
	// holds are all live, and the gallery table is untouched. The blockers ask for
	// EXCLUSIVE, which conflicts with the row-share, row-exclusive and share
	// holds the completion carries, so each one queuing is one live hold.
	for _, table := range []string{"profile_upload_receipt", "profile_photo_state", "files"} {
		if !profileHoldsRelation(t, ctx, observer, session.Pid, table) {
			t.Fatalf("across the Put the session holds no lock on %s", table)
		}
	}
	if profileHoldsRelation(t, ctx, observer, session.Pid, "user_photos") {
		t.Fatalf("across the Put the session already locks the gallery table")
	}

	type profileBlocker struct {
		conn *pgx.Conn
		pid  int
	}
	tables := []string{"profile_upload_receipt", "profile_photo_state", "files"}
	blockers := make([]profileBlocker, 0, len(tables))
	blocked := make(chan error, len(tables))
	for _, table := range tables {
		conn, err := pgx.Connect(ctx, f.dsn)
		if err != nil {
			t.Fatalf("connect blocker for %s: %v", table, err)
		}
		var pid int
		if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatalf("backend pid: %v", err)
		}
		blockers = append(blockers, profileBlocker{conn: conn, pid: pid})
		go func(conn *pgx.Conn, table string) {
			// LOCK TABLE needs a transaction block, and the transaction is what
			// holds the acquired lock: the blocker keeps it until the test rolls
			// it back after the completion lands.
			if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
				blocked <- err
				return
			}
			_, err := conn.Exec(ctx, fmt.Sprintf("LOCK TABLE %s IN EXCLUSIVE MODE", table))
			blocked <- err
		}(conn, table)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		queued := true
		for i, table := range tables {
			if !profileBlockedOnRelation(t, ctx, observer, blockers[i].pid, table) {
				queued = false
			}
		}
		if queued {
			break
		}
		if time.Now().After(deadline) {
			pids := make([]int, 0, 1+len(blockers))
			pids = append(pids, session.Pid)
			for _, b := range blockers {
				pids = append(pids, b.pid)
			}
			var answered string
			for {
				select {
				case err := <-blocked:
					answered += fmt.Sprintf(" blocked-err=%v", err)
					continue
				default:
				}
				break
			}
			t.Fatalf("the queue did not form; pids=%v%s; locks: %s", pids, answered, profileLockLines(t, ctx, observer, pids))
		}
		time.Sleep(10 * time.Millisecond)
	}

	releaseOnce()
	for range tables {
		select {
		case err := <-blocked:
			if err != nil {
				t.Fatalf("a conflicting writer was never admitted: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a conflicting writer did not get its lock after the completion")
		}
	}
	for _, b := range blockers {
		if _, err := b.conn.Exec(ctx, "ROLLBACK"); err != nil {
			t.Fatalf("release blocker: %v", err)
		}
		if err := b.conn.Close(ctx); err != nil {
			t.Errorf("close blocker: %v", err)
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the upload did not finish")
	}
	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.storedRows != 1 || snap.galleryRows != 1 || snap.receiptState != 1 {
		t.Fatalf("the completion did not land: %+v", snap)
	}
}

// TestProfileUploadReceiptPrecedesPartsAllocationAndCap pins what the receipt
// lookup buys: a completed key is answered with the parts gone and the gallery
// full, and a new key over the cap is refused before any allocation, charge or
// Put.
func TestProfileUploadReceiptPrecedesPartsAllocationAndCap(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()

	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))
	if _, err := f.upload(7, 2, profileGalleryLimit, 1<<30); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	if _, err := f.s.DeleteUploadParts(ctx, f.owner, 7); err != nil {
		t.Fatalf("clear parts: %v", err)
	}

	// The completed key, at the cap, with no parts: answered, not refused.
	res, err := f.upload(7, 2, 1, 1<<30)
	if err != nil {
		t.Fatalf("completed key at the cap with the parts gone: %v", err)
	}
	if !res.Replayed {
		t.Fatal("the completed key was not answered from the receipt")
	}

	// A new key over the cap: refused, with nothing allocated, charged
	// or written.
	profilePartsT(t, f.s, f.owner, 8, part('c', 100), part('d', 100))
	_, capErr := f.upload(8, 2, 1, 1<<30)
	if !errors.Is(capErr, store.ErrProfileGalleryCap) {
		t.Fatalf("over cap: want ErrProfileGalleryCap, got %v", capErr)
	}
	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 8)
	if snap.receiptFound {
		t.Fatalf("a cap refusal created a receipt for the new key: %+v", snap)
	}
	if snap.fileRows != 1 {
		t.Fatalf("a cap refusal allocated a file row: files=%d", snap.fileRows)
	}
	if snap.chargedBytes != profilePhotoSize {
		t.Fatalf("a cap refusal moved the lifetime charge to %d bytes", snap.chargedBytes)
	}
	if puts := len(f.blobs.filePuts()); puts != 1 {
		t.Fatalf("a cap refusal produced %d Puts, want 1", puts)
	}
}

// TestProfileUploadFailedPutLeavesNoLiveGallery is the failure criterion: the
// blob write fails and nothing about the upload is visible. No gallery entry, no
// selection change, no stored row, no object in the blob store.
func TestProfileUploadFailedPutLeavesNoLiveGallery(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	failed := errors.New("blob put: simulated backend failure")
	first, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, 1<<30,
		func(store.ProfileAssembly) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{}, failed
		}))
	if !errors.Is(err, failed) {
		t.Fatalf("want the Put failure, got %v", err)
	}
	if first.File.ID != 0 {
		t.Fatalf("a failed Put returned identity %d", first.File.ID)
	}

	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.galleryRows != 0 {
		t.Fatalf("a failed Put created %d gallery entries", snap.galleryRows)
	}
	if snap.storedRows != 0 {
		t.Fatalf("a failed Put stored %d file rows", snap.storedRows)
	}
	if snap.currentFile.Valid {
		t.Fatalf("a failed Put selected a current photo: %v", snap.currentFile)
	}
	if snap.revision != 0 {
		t.Fatalf("a failed Put advanced the revision to %d", snap.revision)
	}
	if !snap.receiptFound || snap.receiptState != 0 {
		t.Fatalf("a failed Put left the receipt at state %d (found %v), want pending", snap.receiptState, snap.receiptFound)
	}
	if puts := len(f.blobs.filePuts()); puts != 0 {
		t.Fatalf("a failed Put produced %d Puts", puts)
	}
	if f.blobHas(snap.receiptFile.Int64) {
		t.Fatalf("a failed Put left an object for file %d", snap.receiptFile.Int64)
	}
}

// TestProfileUploadInvalidDimensionsLeavesNoLiveGallery is the same guarantee on
// the other completion failure: the bytes were written, the validation said no,
// and the published state is unchanged.
func TestProfileUploadInvalidDimensionsLeavesNoLiveGallery(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	_, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, 1<<30,
		func(a store.ProfileAssembly) (store.PhotoDimensions, error) {
			if _, err := profileWriteBlob(f.s, a); err != nil {
				return store.PhotoDimensions{}, err
			}
			return store.PhotoDimensions{Width: 0, Height: 0}, nil
		}))
	if !errors.Is(err, store.ErrInvalidPhotoDimensions) {
		t.Fatalf("want ErrInvalidPhotoDimensions, got %v", err)
	}
	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.galleryRows != 0 || snap.storedRows != 0 || snap.currentFile.Valid {
		t.Fatalf("a rejected photo published state: %+v", snap)
	}
	if snap.receiptState != 0 {
		t.Fatalf("a rejected photo left the receipt in state %d", snap.receiptState)
	}
}

// TestProfileUploadPendingRestartReusesChargedRow is the restart criterion: the
// assembly died after the allocation, and the restart reuses the row that already
// carries the charge and the assembly claim, so the lifetime charge stays at one
// photo.
func TestProfileUploadPendingRestartReusesChargedRow(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	failed := errors.New("blob put: crashed mid-write")
	if _, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, profilePhotoSize,
		func(store.ProfileAssembly) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{}, failed
		})); !errors.Is(err, failed) {
		t.Fatalf("want the assembly failure, got %v", err)
	}
	charged := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if charged.chargedBytes != profilePhotoSize {
		t.Fatalf("the pending row's charge is %d bytes, want %d", charged.chargedBytes, profilePhotoSize)
	}
	if !charged.receiptFound || charged.receiptState != 0 || !charged.receiptFile.Valid {
		t.Fatalf("the receipt is not a pending row naming its file: %+v", charged)
	}
	fileID := charged.receiptFile.Int64

	// A new process, as a restart is: same database, same parts, no memory of the
	// dead assembly.
	restarted := profileStoreOn(t, f.blobs, f.dsn)
	res, err := restarted.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, profilePhotoSize,
		func(a store.ProfileAssembly) (store.PhotoDimensions, error) { return profileWriteBlob(restarted, a) }))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if res.File.ID != fileID {
		t.Fatalf("the restart allocated file %d; the pending receipt's row is %d", res.File.ID, fileID)
	}
	after := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if after.fileRows != 1 {
		t.Fatalf("the restart left %d file rows, want 1", after.fileRows)
	}
	if after.chargedBytes != profilePhotoSize {
		t.Fatalf("the restart moved the lifetime charge to %d bytes", after.chargedBytes)
	}
	if after.receiptState != 1 || after.galleryRows != 1 || after.currentFile.Int64 != fileID {
		t.Fatalf("the restart did not complete: %+v", after)
	}
	if puts := len(f.blobs.filePuts()); puts != 1 {
		t.Fatalf("the restart sequence wrote %d objects, want 1", puts)
	}
	if !f.blobHas(fileID) {
		t.Fatalf("the restart did not write file %d's bytes", fileID)
	}
}

// TestProfileUploadPendingRestartAllocatesOnlyWhenChargedRowAbsent is the other
// half of the restart rule: the row was aged out, so its charge is already gone,
// and only then may a new row be allocated.
func TestProfileUploadPendingRestartAllocatesOnlyWhenChargedRowAbsent(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	failed := errors.New("blob put: crashed mid-write")
	if _, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, profilePhotoSize,
		func(store.ProfileAssembly) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{}, failed
		})); !errors.Is(err, failed) {
		t.Fatalf("want the assembly failure, got %v", err)
	}
	before := profileSnapshot(t, ctx, f.dsn, f.owner, 7)

	// The eraser retires the aged unstored row. The future cutoff is what the
	// eraser's own tests use, and the receipt's foreign key is what nulls the
	// pointer.
	if _, err := f.s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch); err != nil {
		t.Fatalf("erasure sweep: %v", err)
	}
	aged := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if aged.fileRows != 0 {
		t.Fatalf("the age-out sweep left %d file rows", aged.fileRows)
	}
	if aged.chargedBytes != 0 {
		t.Fatalf("the age-out sweep left a charge of %d bytes", aged.chargedBytes)
	}
	if aged.receiptRows != 1 || aged.receiptFile.Valid {
		t.Fatalf("the receipt did not survive with a null pointer: %+v", aged)
	}

	restarted := profileStoreOn(t, f.blobs, f.dsn)
	res, err := restarted.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, 1<<30,
		func(a store.ProfileAssembly) (store.PhotoDimensions, error) { return profileWriteBlob(restarted, a) }))
	if err != nil {
		t.Fatalf("restart after age-out: %v", err)
	}
	if res.File.ID == before.receiptFile.Int64 {
		t.Fatalf("the restart reused the retired row %d", res.File.ID)
	}
	after := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if after.fileRows != 1 || after.storedRows != 1 {
		t.Fatalf("after the age-out restart: %+v", after)
	}
	if after.receiptState != 1 || after.receiptFile.Int64 != res.File.ID {
		t.Fatalf("the receipt did not move onto the new row: %+v", after)
	}
	if after.galleryRows != 1 || after.currentFile.Int64 != res.File.ID {
		t.Fatalf("the age-out restart did not publish: %+v", after)
	}
}

// TestProfileUploadTerminalReceiptRejectsRetryUniformly is the deleted-photo
// criterion: the receipt outlives the gallery entry, rejects a retry with the
// same answer a deleted key always gets, keeps its identifiers, and does not pin
// the photo's bytes against reclamation.
func TestProfileUploadTerminalReceiptRejectsRetryUniformly(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))
	res, err := f.upload(7, 2, 0, 1<<30)
	if err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	// What the gallery delete slice will do: clear the pointer, remove the entry,
	// terminalize the receipt with its file reference dropped.
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `UPDATE profile_photo_state SET current_file_id = NULL WHERE user_id = $1`, f.owner); err != nil {
		t.Fatalf("clear current: %v", err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM user_photos WHERE user_id = $1 AND client_file_id = 7`, f.owner); err != nil {
		t.Fatalf("delete gallery entry: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		UPDATE profile_upload_receipt
		SET state = 2, file_id = NULL, updated_at = now()
		WHERE user_id = $1 AND client_file_id = 7`, f.owner); err != nil {
		t.Fatalf("terminalize receipt: %v", err)
	}

	// The terminal answer is uniform: the same-shape retry and a retry
	// that redeclares the request under the deleted key get the same error, so a
	// deleted key never reports what its recorded request was.
	_, deletedErr := f.upload(7, 2, 0, 1<<30)
	_, deletedChangedShape := f.upload(7, 3, 0, 1<<30)
	for name, err := range map[string]error{
		"same shape":    deletedErr,
		"changed count": deletedChangedShape,
	} {
		if !errors.Is(err, store.ErrProfilePhotoUnavailable) {
			t.Fatalf("retry of a deleted key (%s): want ErrProfilePhotoUnavailable, got %v", name, err)
		}
	}

	// A fresh key is not refused like a deleted one.
	profilePartsT(t, f.s, f.owner, 9, part('e', 100), part('f', 100))
	if _, err := f.upload(9, 2, 0, 1<<30); err != nil {
		t.Fatalf("fresh key after a terminal receipt: %v", err)
	}

	// The retry of the deleted key wrote nothing.
	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.fileRows != 2 {
		t.Fatalf("the deleted-key retry allocated a row: files=%d", snap.fileRows)
	}
	if snap.galleryRows != 1 {
		t.Fatalf("the deleted-key retry created a gallery entry: %d", snap.galleryRows)
	}
	if puts := len(f.blobs.filePuts()); puts != 2 {
		t.Fatalf("the deleted-key retry wrote %d objects, want the 2 uploads only", puts)
	}

	// The terminal receipt keeps its identifiers and its
	// fingerprint, and the file it named is reclaimable: the receipt's foreign
	// key nulls, it does not restrict.
	if _, err := conn.Exec(ctx, `DELETE FROM files WHERE id = $1`, res.File.ID); err != nil {
		t.Fatalf("reclaim the file the terminal receipt names: %v", err)
	}
	var (
		state       int16
		fileID      sql.NullInt64
		size        int64
		parts       int32
		digestBytes int
	)
	if err := conn.QueryRow(ctx, `
		SELECT state, file_id, request_size, part_count, octet_length(payload_digest)
		FROM profile_upload_receipt WHERE user_id = $1 AND client_file_id = 7`, f.owner).
		Scan(&state, &fileID, &size, &parts, &digestBytes); err != nil {
		t.Fatalf("read the terminal receipt: %v", err)
	}
	if state != 2 {
		t.Fatalf("terminal receipt state = %d", state)
	}
	if fileID.Valid {
		t.Fatalf("the terminal receipt still pins file %d", fileID.Int64)
	}
	if size != profilePhotoSize || parts != 2 || digestBytes != 32 {
		t.Fatalf("the terminal receipt lost its identifiers: size=%d parts=%d digest=%d bytes", size, parts, digestBytes)
	}
}

// TestProfileUploadIntroducesNoReceiptCompaction pins the negative: nothing this
// slice adds retires a receipt. The upload, its retry, the part sweep and the
// media erasure all leave the row in place, so the retry identity survives as
// long as the schema says it must.
func TestProfileUploadIntroducesNoReceiptCompaction(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))
	if _, err := f.upload(7, 2, 0, 1<<30); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	if _, err := f.upload(7, 2, 0, 1<<30); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if _, err := f.s.SweepExpiredUploadParts(ctx, future(), store.ExpiredPartSweepBatch); err != nil {
		t.Fatalf("part sweep: %v", err)
	}
	if _, err := f.s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch); err != nil {
		t.Fatalf("media erasure: %v", err)
	}

	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if snap.receiptRows != 1 {
		t.Fatalf("receipts after the sweeps = %d, want 1", snap.receiptRows)
	}
	if snap.receiptState != 1 {
		t.Fatalf("the sweeps moved the receipt to state %d", snap.receiptState)
	}
	if snap.galleryRows != 1 {
		t.Fatalf("the media erasure removed a gallery-referenced file: %+v", snap)
	}
}

// TestProfileUploadLeavesNoAdvisoryLockBehind is the cleanup rule the
// lane's own shape forces: it pins one session for a whole upload and takes two
// session-scoped advisory locks on it, so every one of them has to be gone
// when the call returns. The aged-out restart is in the sequence because that is
// the path that moves a per-file claim onto a new row.
func TestProfileUploadLeavesNoAdvisoryLockBehind(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	failed := errors.New("blob put: crashed mid-write")
	if _, err := f.s.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, profilePhotoSize,
		func(store.ProfileAssembly) (store.PhotoDimensions, error) {
			return store.PhotoDimensions{}, failed
		})); !errors.Is(err, failed) {
		t.Fatalf("want the first assembly to fail, got %v", err)
	}
	if _, err := f.s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch); err != nil {
		t.Fatalf("erasure sweep: %v", err)
	}
	restarted := profileStoreOn(t, f.blobs, f.dsn)
	if _, err := restarted.ProfileUpload(ctx, profileRequest(f.owner, 7, 2, 0, 1<<30,
		func(a store.ProfileAssembly) (store.PhotoDimensions, error) { return profileWriteBlob(restarted, a) })); err != nil {
		t.Fatalf("restart after age-out: %v", err)
	}
	if _, err := f.upload(7, 2, 0, 1<<30); err != nil {
		t.Fatalf("completed retry: %v", err)
	}

	observer, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect observer: %v", err)
	}
	defer func() { _ = observer.Close(ctx) }() //nolint:errcheck // best-effort close

	// The profile-domain holds (the two-key space) and the assembly claims
	// (negative keys in the one-key space) in this test's database, on any session
	// connected to it, after every upload call has returned.
	leaked := profileCount(t, ctx, observer, `
		SELECT count(*)
		FROM pg_locks l
		WHERE l.locktype = 'advisory'
		  AND l.pid IN (SELECT pid FROM pg_stat_activity WHERE datname = current_database())
		  AND ((l.objsubid = 2 AND l.classid::bigint = $1)
		    OR (l.objsubid = 1 AND l.classid::bigint = $2))`,
		int64(store.ProfileLockDomain), profileNegativeKeyClass)
	if leaked != 0 {
		t.Fatalf("%d profile-domain or assembly-claim advisory locks are still held by idle sessions", leaked)
	}
}

// profileLocalBlobs is a counting view of one shared local blob directory. The
// two-process test uses it in every process, so the object set is one
// system's, not one process's.
func profileLocalBlobs(tb testing.TB, dir string) *profileBlobs {
	tb.Helper()
	b, err := blob.NewLocal(dir)
	if err != nil {
		tb.Fatalf("local blob dir %s: %v", dir, err)
	}
	return &profileBlobs{Store: b}
}

// profileAssembledObjects counts the objects in a blob directory that are not
// upload parts: the whole-system Put count.
func profileAssembledObjects(tb testing.TB, dir string) int {
	tb.Helper()
	count := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(filepath.ToSlash(rel), blob.PartsPrefix) {
			count++
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("walk blob dir: %v", err)
	}
	return count
}

// TestProfileUploadPartReplacementDuringMeasurement closes the part-replacement
// window in the measurement. A part save replaces a row's size, moves the row to
// a new object, and deletes the object the parts snapshot named, so a save that
// lands between the snapshot and the digest pass has to end the upload before
// anything is charged. What must never happen is a receipt recording a size read
// at one instant and a digest read at another: that pair names a fingerprint no
// measurement can ever reproduce, so the key's coherent retry would be rejected
// for the rest of the key's life. The quota here is not the subject, so it is
// roomy; the one-photo quota is the two-process test's job.
func TestProfileUploadPartReplacementDuringMeasurement(t *testing.T) {
	t.Parallel()
	f := newProfileFixture(t)
	ctx := context.Background()
	profilePartsT(t, f.s, f.owner, 7, part('a', 100), part('b', 100))

	// The seam lives on this test's Store, so no parallel gallery test's upload
	// consumes the trigger and no fixture other than this one is written to.
	replaced := false
	prev := store.ProfilePartsSnapshotHook(f.s, func(ownerID, clientFileID int64) {
		if replaced {
			return
		}
		replaced = true
		if err := f.s.SaveUploadPart(ctx, ownerID, clientFileID, 0, part('c', 120), profilePerFileCap); err != nil {
			f.t.Errorf("replace part 0 mid-measurement: %v", err)
		}
	})
	t.Cleanup(func() { store.ProfilePartsSnapshotHook(f.s, prev) })

	const quota = 1 << 20
	res1, err1 := f.upload(7, 2, 1, quota)
	snap := profileSnapshot(t, ctx, f.dsn, f.owner, 7)
	if err1 == nil {
		// A success is only legitimate when the recorded size is the size
		// the digest covered: one set of parts, not two.
		if !snap.receiptFound || snap.receiptSize != 220 || snap.receiptParts != 2 {
			t.Fatalf("receipt recorded size=%d parts=%d: a size and a digest drawn from two instants can never be measured again",
				snap.receiptSize, snap.receiptParts)
		}
	} else {
		// The outcome the design takes: the digest pass refused the part whose
		// object the save deleted, and it refused before the allocation. Nothing
		// was charged and no receipt exists, so the key is clean.
		if !errors.Is(err1, store.ErrUploadPartMissing) {
			t.Fatalf("mid-measurement replacement: want a refused part read, got %v", err1)
		}
		if snap.receiptFound || snap.fileRows != 0 || snap.galleryRows != 0 {
			t.Fatalf("a refused measurement left state behind: receipt=%v files=%d gallery=%d",
				snap.receiptFound, snap.fileRows, snap.galleryRows)
		}
		if puts := f.blobs.filePuts(); len(puts) != 0 {
			t.Fatalf("a refused measurement wrote %d file objects", len(puts))
		}
	}

	// The key is not poisoned by its own measurement: a coherent retry of the
	// parts as they stand is served, and it is the one charge.
	res2, err := f.upload(7, 2, 1, quota)
	if err != nil {
		t.Fatalf("coherent retry after a mid-measurement replacement: %v (conflict=%v)",
			err, errors.Is(err, store.ErrProfileUploadConflict))
	}
	if err1 == nil && res1.File.ID != res2.File.ID {
		t.Fatalf("the retry served a different identity: first upload %d, retry %d", res1.File.ID, res2.File.ID)
	}
	if snap = profileSnapshot(t, ctx, f.dsn, f.owner, 7); snap.fileRows != 1 || snap.storedRows != 1 ||
		snap.chargedBytes != 220 || snap.galleryRows != 1 || !snap.receiptFound ||
		snap.receiptState != 1 || snap.receiptSize != 220 || snap.receiptParts != 2 || snap.revision != 1 {
		t.Fatalf("state after the coherent retry: %+v", snap)
	}
	if !snap.currentFile.Valid || snap.currentFile.Int64 != res2.File.ID {
		t.Fatalf("current selection = %v, want the served file %d", snap.currentFile, res2.File.ID)
	}
	if puts := f.blobs.filePuts(); len(puts) != 1 {
		t.Fatalf("file Puts across the measurement and the retry = %d (%v), want 1", len(puts), puts)
	}
	if !f.blobHas(res2.File.ID) {
		t.Fatalf("the served file %d has no object", res2.File.ID)
	}
}

// The two-process criterion, run as two OS processes. Two Store instances in one
// process share an address space, a lock-table view and a Put counter, so they
// cannot show that the serialization survives a real process
// boundary. Here each child is its own process, with its own pool and its own
// counting blob view, over one Postgres database and one blob directory. The
// parent gates their start and watches the lock table, so the overlap is
// observed: both children enter the call, one is inside the Put while the other
// is queued behind it on the profile-domain advisory key, and neither has left
// before the other entered.
const (
	profileChildEnv  = "TG_PROFILE_CHILD"
	profileChildMark = "PROFILE_CHILD_RESULT "
	profileChildFile = 7
)

// profileChildConfig is what a child needs to join the parent's world.
type profileChildConfig struct {
	label, dsn, blobs, rdv, app string
	owner                       int64
}

func profileChildFromEnv() (profileChildConfig, bool) {
	if os.Getenv(profileChildEnv) == "" {
		return profileChildConfig{}, false
	}
	owner, err := strconv.ParseInt(os.Getenv("TG_PROFILE_OWNER"), 10, 64)
	if err != nil || owner <= 0 {
		return profileChildConfig{}, false
	}
	return profileChildConfig{
		label: os.Getenv("TG_PROFILE_LABEL"),
		dsn:   os.Getenv("TG_PROFILE_DSN"),
		blobs: os.Getenv("TG_PROFILE_BLOBS"),
		rdv:   os.Getenv("TG_PROFILE_RDV"),
		app:   os.Getenv("TG_PROFILE_APP"),
		owner: owner,
	}, true
}

// profileDSNWithApp appends an application_name parameter to a DSN, the way the
// repo's lock observers name the sessions they watch.
func profileDSNWithApp(dsn, app string) string {
	separator := "&"
	if !strings.Contains(dsn, "?") {
		separator = "?"
	}
	return dsn + separator + "application_name=" + app
}

// profileChildResult is one child's answer, printed on stdout for the parent.
type profileChildResult struct {
	Label    string `json:"label"`
	Err      string `json:"err,omitempty"`
	FileID   int64  `json:"file_id"`
	Revision int64  `json:"revision"`
	Replayed bool   `json:"replayed"`
	Puts     int    `json:"puts"`
}

func (r profileChildResult) line() string {
	out, err := json.Marshal(r)
	if err != nil {
		return profileChildMark + `{"label":"` + r.Label + `","err":"marshal: ` + err.Error() + `"}`
	}
	return profileChildMark + string(out)
}

// profileRendezvous is the parent's line protocol with the children. One unix
// socket per test: a child announces itself, the parent releases it, and the
// same connection carries the child's progress messages and the parent's
// commands. A connection stays open for the whole upload, so what the parent
// sees on it is timed against the child's call.
type profileRendezvous struct {
	ln    net.Listener
	path  string
	lines chan profileLine
	mu    sync.Mutex
	conns map[string]net.Conn
}

type profileLine struct {
	label  string
	msg    string
	detail string
}

func newProfileRendezvous(t *testing.T) *profileRendezvous {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rdv.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatalf("rendezvous listen: %v", err)
	}
	r := &profileRendezvous{ln: ln, path: path, lines: make(chan profileLine, 64), conns: map[string]net.Conn{}}
	t.Cleanup(func() { _ = ln.Close() }) //nolint:errcheck // the test is over
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // the listener closed with the test
			}
			go r.read(conn)
		}
	}()
	return r
}

func (r *profileRendezvous) read(conn net.Conn) {
	defer func() { _ = conn.Close() }() //nolint:errcheck // the child is done talking
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		label, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue // the parent's own command echo, or a partial write
		}
		msg, detail, _ := strings.Cut(rest, " ")
		if msg == "armed" {
			r.mu.Lock()
			r.conns[label] = conn
			r.mu.Unlock()
		}
		r.lines <- profileLine{label: label, msg: msg, detail: detail}
	}
}

func (r *profileRendezvous) send(t *testing.T, label, msg string) {
	t.Helper()
	r.mu.Lock()
	conn, ok := r.conns[label]
	r.mu.Unlock()
	if !ok {
		t.Fatalf("child %s never announced itself", label)
	}
	if _, err := conn.Write([]byte(msg + "\n")); err != nil {
		t.Fatalf("send %q to %s: %v", msg, label, err)
	}
}

func (r *profileRendezvous) next(t *testing.T, within time.Duration) profileLine {
	t.Helper()
	select {
	case l := <-r.lines:
		return l
	case <-time.After(within):
		t.Fatalf("timed out after %v waiting for a child message", within)
	}
	return profileLine{}
}

// TestProfileUploadProcessChild is the child entry point for
// TestProfileUploadTwoProcessRetryObservedOverlap. It runs the real upload in its
// own process, announcing each step on the parent's socket: it is released,
// enters the call, and where it reaches the Put it stops inside it, holding the
// profile-domain lock, until the parent has read the lock table.
func TestProfileUploadProcessChild(t *testing.T) {
	cfg, ok := profileChildFromEnv()
	if !ok {
		t.Skip("child entry point: runs only as a child of TestProfileUploadTwoProcessRetryObservedOverlap")
	}
	ctx := context.Background()
	blobs := profileLocalBlobs(t, cfg.blobs)
	// The application_name rides on every connection the child's store opens, so
	// the parent's lock observer can attribute a row to this child.
	s, err := store.Open(ctx, profileDSNWithApp(cfg.dsn, cfg.app), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		fmt.Println(profileChildResult{Label: cfg.label, Err: "open: " + err.Error()}.line())
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // best-effort close

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", cfg.rdv)
	if err != nil {
		fmt.Println(profileChildResult{Label: cfg.label, Err: "dial: " + err.Error()}.line())
		t.Fatalf("dial the rendezvous: %v", err)
	}
	defer func() { _ = conn.Close() }() //nolint:errcheck // the parent reads what was written
	ch := &profileChildChannel{label: cfg.label, conn: conn, w: bufio.NewWriter(conn), r: bufio.NewReader(conn)}
	fail := func(what string, err error) {
		fmt.Println(profileChildResult{Label: cfg.label, Err: what + ": " + err.Error()}.line())
		t.Fatalf("%s: %v", what, err)
	}
	if err := ch.send("armed"); err != nil {
		fail("announce", err)
	}
	if err := ch.expect("go", 3*time.Minute); err != nil {
		fail("wait for the start gate", err)
	}
	if err := ch.send("in-call"); err != nil {
		fail("announce the call", err)
	}

	res, err := s.ProfileUpload(ctx, profileRequest(cfg.owner, profileChildFile, 2, 1, profilePhotoSize,
		func(a store.ProfileAssembly) (store.PhotoDimensions, error) {
			// The Put is the long part of the critical section. Stopping
			// inside it is what lets the parent read the lock table while the
			// other process is queued behind this one.
			if e := ch.send("in-put"); e != nil {
				return store.PhotoDimensions{}, e
			}
			if e := ch.expect("release-put", 3*time.Minute); e != nil {
				return store.PhotoDimensions{}, e
			}
			return profileWriteBlob(s, a)
		}))

	out := profileChildResult{Label: cfg.label, Puts: len(blobs.filePuts())}
	if err != nil {
		out.Err = err.Error()
	} else {
		out.FileID, out.Revision, out.Replayed = res.File.ID, res.MutationRevision, res.Replayed
	}
	fmt.Println(out.line())
	if e := ch.send("result " + strings.TrimSpace(strings.TrimPrefix(out.line(), profileChildMark))); e != nil {
		fail("report the result", e)
	}
	if err != nil {
		t.Fatalf("child upload: %v", err)
	}
}

// profileChildChannel is one unix connection, line-oriented in both
// directions. Writes are flushed per message so the parent sees each step as the
// child reaches it.
type profileChildChannel struct {
	label string
	conn  net.Conn
	w     *bufio.Writer
	r     *bufio.Reader
}

// send prefixes the child's label, so the parent can tell its children apart on
// one socket.
func (c *profileChildChannel) send(msg string) error {
	if _, err := c.w.WriteString(c.label + " " + msg + "\n"); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *profileChildChannel) expect(want string, within time.Duration) error {
	if err := c.conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return err
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != want {
		return fmt.Errorf("got %q, want %q", strings.TrimSpace(line), want)
	}
	return nil
}

// profileAdvisoryByApp counts the profile-domain advisory locks of the named
// sessions in the observer's own database, granted or not as asked.
func profileAdvisoryByApp(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64, granted bool, appNames []string) int64 {
	t.Helper()
	return profileCount(t, ctx, conn, `
		SELECT count(*)
		FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.objsubid = 2
		  AND l.classid::bigint = $1 AND l.objid::bigint = $2
		  AND l.granted = $3
		  AND a.datname = current_database()
		  AND a.application_name = ANY($4::text[])`,
		int64(store.ProfileLockDomain), ownerID, granted, appNames)
}

func TestProfileUploadTwoProcessRetryObservedOverlap(t *testing.T) {
	if testing.Short() {
		t.Skip("two OS processes: skipped under -short")
	}
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobsDir := t.TempDir()

	// The parent owns the setup the children share: one owner, one key's parts,
	// one blob directory. It never uploads, so every Put is a child's.
	parentBlobs := profileLocalBlobs(t, blobsDir)
	ps := profileStoreOn(t, parentBlobs, dsn)
	owner, err := ps.CreateUser(ctx, "+15559300143")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ownerID := owner.ID
	profilePartsT(t, ps, ownerID, profileChildFile, part('a', 100), part('b', 100))

	rdv := newProfileRendezvous(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary: %v", err)
	}
	type childProc struct {
		label string
		app   string
		out   *bytes.Buffer
		cmd   *exec.Cmd
	}
	const children = 2
	procs := make([]childProc, 0, children)
	for i := range children {
		label := fmt.Sprintf("child%d", i+1)
		app := "profile-upload-child-" + label
		out := &bytes.Buffer{}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		t.Cleanup(cancel)
		//nolint:gosec // G204: exe is this test binary and the arguments are fixed flags
		cmd := exec.CommandContext(cctx, exe, "-test.run=TestProfileUploadProcessChild", "-test.count=1")
		cmd.Env = append(os.Environ(),
			profileChildEnv+"=1",
			"TG_PROFILE_LABEL="+label,
			"TG_PROFILE_DSN="+dsn,
			"TG_PROFILE_BLOBS="+blobsDir,
			"TG_PROFILE_OWNER="+strconv.FormatInt(ownerID, 10),
			"TG_PROFILE_RDV="+rdv.path,
			"TG_PROFILE_APP="+app)
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", label, err)
		}
		procs = append(procs, childProc{label: label, app: app, out: out, cmd: cmd})
	}
	t.Cleanup(func() {
		for _, p := range procs {
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill() //nolint:errcheck // the test is over
			}
		}
	})

	for range procs {
		if l := rdv.next(t, 2*time.Minute); l.msg != "armed" {
			t.Fatalf("child %s sent %q before the start gate, want armed", l.label, l.msg)
		}
	}
	for _, p := range procs {
		rdv.send(t, p.label, "go")
	}

	// The overlap, observed. Collect until both distinct children have entered
	// the call and one has announced it is inside the Put: a child scheduled
	// later is not a failure, and the winner cannot finish before the parent
	// releases the paused Put, so a result arriving here is a real break.
	inCall := map[string]bool{}
	putLabel := ""
	for len(inCall) < children || putLabel == "" {
		l := rdv.next(t, 2*time.Minute)
		switch l.msg {
		case "in-call":
			inCall[l.label] = true
		case "in-put":
			if putLabel != "" {
				t.Fatalf("both processes reached the Put (%s and %s), so the profile domain did not serialize them", putLabel, l.label)
			}
			putLabel = l.label
		case "result":
			t.Fatalf("child %s ended before the overlap was established: entered=%v put=%s", l.label, inCall, putLabel)
		default:
			t.Fatalf("unexpected message %q from %s", l.msg, l.label)
		}
	}

	// The lock table agrees: one process holds the profile-domain key and the
	// other is queued on it, while the holder is inside the blob Put. pg_locks
	// spans the whole cluster, so the counts are narrowed twice: to this
	// database, and to the two children's own backends, named by the
	// application_name each carries on every connection. Parallel fixtures reuse
	// owner ids in other databases, and a session name is the closest handle
	// Postgres gives a test to a specific client's backends.
	obs, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("observer connect: %v", err)
	}
	defer func() { _ = obs.Close(ctx) }() //nolint:errcheck // best-effort close
	childrenApp := make([]string, 0, children)
	for _, p := range procs {
		childrenApp = append(childrenApp, p.app)
	}
	profileWait(t, "one process queued behind the other on the profile domain key", func() bool {
		return profileAdvisoryByApp(t, ctx, obs, ownerID, true, childrenApp) >= 1 &&
			profileAdvisoryByApp(t, ctx, obs, ownerID, false, childrenApp) >= 1
	})
	rdv.send(t, putLabel, "release-put")

	results := map[string]profileChildResult{}
	for len(results) < children {
		l := rdv.next(t, 3*time.Minute)
		if l.msg != "result" {
			t.Fatalf("child %s sent %q, want the result", l.label, l.msg)
		}
		var r profileChildResult
		if err := json.Unmarshal([]byte(l.detail), &r); err != nil {
			t.Fatalf("child %s result: %v", l.label, err)
		}
		results[l.label] = r
	}
	for _, p := range procs {
		if err := p.cmd.Wait(); err != nil {
			t.Errorf("child %s: %v\n%s", p.label, err, p.out.String())
		}
	}

	got := make([]profileChildResult, 0, children)
	totalPuts := 0
	for _, p := range procs {
		r, ok := results[p.label]
		if !ok {
			t.Fatalf("child %s reported no result\n%s", p.label, p.out.String())
		}
		if r.Err != "" {
			t.Fatalf("child %s: %s\n%s", p.label, r.Err, p.out.String())
		}
		got = append(got, r)
		totalPuts += r.Puts
	}
	if got[0].FileID != got[1].FileID || got[0].Revision != got[1].Revision {
		t.Fatalf("the two processes did not return the same identity: %+v", got)
	}
	replayed := 0
	for _, r := range got {
		if r.Replayed {
			replayed++
		}
	}
	if replayed != 1 {
		t.Fatalf("exactly one process must replay the completed key: %+v", got)
	}
	if totalPuts != 1 {
		t.Fatalf("file Puts across both processes = %d, want 1: %+v", totalPuts, got)
	}
	if objects := profileAssembledObjects(t, blobsDir); objects != 1 {
		t.Fatalf("assembled objects in the shared blob dir = %d, want 1", objects)
	}
	if parentBlobs.filePuts() != nil && len(parentBlobs.filePuts()) != 0 {
		t.Fatalf("the parent wrote a Put: %v", parentBlobs.filePuts())
	}
	snap := profileSnapshot(t, ctx, dsn, ownerID, profileChildFile)
	if snap.fileRows != 1 || snap.storedRows != 1 || snap.chargedBytes != profilePhotoSize ||
		snap.galleryRows != 1 || snap.receiptRows != 1 || snap.receiptState != 1 || snap.revision != 1 {
		t.Fatalf("state after two processes at one-photo quota: %+v", snap)
	}
	if !snap.currentFile.Valid || snap.currentFile.Int64 != got[0].FileID {
		t.Fatalf("current selection = %v, want the served file %d", snap.currentFile, got[0].FileID)
	}
}
