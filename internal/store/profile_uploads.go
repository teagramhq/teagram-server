package store

// The gallery lane's upload primitive: one client file id, one file row, one
// lifetime quota charge, one gallery entry, and a durable receipt that makes a
// retry answerable from the database alone.
//
// It is deliberately unregistered: no RPC reaches this path, and only synthetic
// callers exercise it. The messaging lane's allocation, authorization and quota
// behaviour is untouched; what is shared with it is allocateFileTx, the assembly
// claim, the files-row reference interlock and the upload parts.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// profileMediaMode is the only media mode this lane accepts, and it is the
// receipt's CHECK constraint: an unsupported mode is refused before any work
// rather than stored.
const profileMediaMode = "photo"

// The receipt states, mirroring the schema's CHECK (state IN (0,1,2)).
const (
	profileReceiptPending  int16 = 0
	profileReceiptComplete int16 = 1
	profileReceiptDeleted  int16 = 2
)

// profileLockDomain is the classid half of the gallery lane's serialization key,
// and it is why the lane cannot collide with messaging.
//
// Postgres keeps the one-argument and two-argument advisory locks in separate key
// spaces, and pg_locks reports them under different objsubid. Every messaging
// advisory lock in this server is the one-argument form: a bare user id from
// lockOwners, and a negative file id for an assembly claim. This lane takes the
// two-argument form (profileLockDomain, owner id), so an owner's profile uploads
// serialize against themselves and against nothing else. The bare messaging owner
// key is never held across a blob Put, which is what would let a stalled storage
// backend stall that owner's inbound DMs and group sends.
const profileLockDomain = int32(0x50463031) // "PF01"

// profileOwnerLockKey narrows an owner id into the two-argument key. It fails
// closed: an id above int32 would alias onto another owner's key, and user ids
// are BIGSERIAL, so reaching that bound is a bug and not a configuration.
func profileOwnerLockKey(ownerID int64) (class, obj int32, ok bool) {
	if ownerID < 1 || ownerID > math.MaxInt32 {
		return 0, 0, false
	}
	return profileLockDomain, int32(ownerID), true
}

// ErrProfileUploadConflict is the one answer for every request that reuses a key
// whose content is not the content that key bought: a different part count, a
// different measured size, a different payload digest, a different mode. They are
// one error on purpose. Splitting them into "no such key", "wrong size" and
// "wrong digest" would make the dedup key a probe that reports what a key's
// recorded request was.
var ErrProfileUploadConflict = errors.New("profile upload request does not match its receipt")

// ErrProfilePhotoUnavailable is the uniform answer for a key whose photo is not
// there to serve: a terminal (deleted) receipt, and a complete receipt whose
// gallery entry is gone. It never says which, so a deleted photo and a key that
// was never uploaded answer the same way, and a retry under a deleted key is
// refused rather than re-uploading a photo its owner deleted.
var ErrProfilePhotoUnavailable = errors.New("profile photo unavailable")

// ErrProfileGalleryCap is returned when admitting a new gallery entry would take
// the owner past the gallery cap.
var ErrProfileGalleryCap = errors.New("profile gallery cap reached")

// ErrProfileUploadNoCapacity refuses a gallery upload on a deployment whose pool
// cannot yield the gallery lane's assembly reserve: the lane needs a pinned
// connection out of the pool's non-assembly share, and a pool that cannot give one
// without taking it from media assembly or ordinary traffic gets a refusal instead
// of a queue.
var ErrProfileUploadNoCapacity = errors.New("profile upload: the assembly budget has no slot the gallery lane can reserve")

// ErrProfilePartsIncomplete is returned when the parts a request names are not a
// complete contiguous set with bytes. It is separate from
// ErrProfileUploadConflict because it says nothing about a stored receipt.
var ErrProfilePartsIncomplete = errors.New("profile upload parts incomplete")

// ErrProfileAssemblyClaimed is returned when the file row this lane is
// assembling is under another session's live assembly claim.
var ErrProfileAssemblyClaimed = errors.New("profile upload assembly is claimed elsewhere")

// errProfileRowAgedOut is the completion's private signal that the files row the
// receipt names is gone. It is never returned to a caller.
var errProfileRowAgedOut = errors.New("profile upload charged row is gone")

// ProfileAssembly is what this lane measured about a request, handed to the
// caller's assembly callback. The caller never reads the parts itself: the
// receipt lookup has to precede every part read, so the parts are measured here
// and passed down.
type ProfileAssembly struct {
	File  File
	Parts []UploadPartRef
	Total int64
}

// ProfileUploadRequest describes one gallery-lane upload.
type ProfileUploadRequest struct {
	OwnerID int64
	// ClientFileID is the client-chosen upload id; (owner, client_file_id) is the
	// dedup key.
	ClientFileID int64
	// Parts is the client-declared part count. It is checked against the receipt
	// before any part is read, and against the measured set afterwards.
	Parts int
	// MimeType and FileName are stored with the row; the caller sanitizes them
	// at its own boundary, as the messaging lane does.
	MimeType string
	FileName string
	// MaxFileBytes bounds the assembled file before its payload is read, so the
	// digest pass cannot be made to stream an unbounded upload.
	MaxFileBytes int64
	// MaxUserBytes is the lifetime storage cap the allocation is admitted
	// against: the same value the messaging lane passes.
	MaxUserBytes int64
	// GalleryCap is the owner's gallery entry cap, admitted as a new entry.
	GalleryCap int
	// Assemble writes the parts' bytes under the allocated file's blob key and
	// returns validated dimensions. It runs under the files-row reference
	// interlock, as every assembly in this server does, and a failure there
	// leaves the row unstored and the receipt pending.
	Assemble func(ProfileAssembly) (PhotoDimensions, error)
}

// ProfileUploadResult is the identity one upload produced. A request answered
// from a completed receipt reports Replayed with the same File, and never
// allocates, writes a blob, changes the selection, or charges quota.
//
// What it reads depends on where the retry arrives. A retry after part cleanup —
// the normal case, whose assembly deleted the parts — is answered from the
// receipt and the gallery row alone, reading no part payload at all. A retry
// whose parts are still present is measured first: the part summary, and a
// SHA-256 pass over the payload, compared against the fingerprint the receipt
// recorded. That measurement is the only part read a replay performs, and it is
// what stops a completed key from being re-pointed at different bytes.
type ProfileUploadResult struct {
	File             File
	ClientFileID     int64
	MutationRevision int64
	Replayed         bool
}

func (r ProfileUploadRequest) validate() error {
	if r.OwnerID <= 0 {
		return fmt.Errorf("profile upload: owner id %d is not positive", r.OwnerID)
	}
	if r.ClientFileID <= 0 {
		return fmt.Errorf("profile upload: client file id %d is not positive", r.ClientFileID)
	}
	if r.Parts <= 0 {
		return fmt.Errorf("profile upload: part count %d is not positive", r.Parts)
	}
	if r.MaxFileBytes <= 0 {
		return fmt.Errorf("profile upload: max file bytes %d is not positive", r.MaxFileBytes)
	}
	if r.MaxUserBytes < 0 {
		return fmt.Errorf("profile upload: max user bytes %d is negative", r.MaxUserBytes)
	}
	if r.GalleryCap < 0 {
		return fmt.Errorf("profile upload: gallery cap %d is negative", r.GalleryCap)
	}
	if r.MimeType == "" || r.FileName == "" {
		return errors.New("profile upload: empty mime type or file name")
	}
	if r.Assemble == nil {
		return errors.New("profile upload: nil assembly callback")
	}
	return nil
}

// profileParts is the measured request: the contiguous part set, its summed size,
// and SHA-256 over the assembled bytes. Every fingerprint column of a receipt
// comes out of here, so nothing the client declared is what a retry is compared
// against.
type profileParts struct {
	refs   []UploadPartRef
	total  int64
	digest [sha256.Size]byte
}

// ProfileUpload serializes one owner's gallery uploads on the profile advisory
// lock domain and lands the whole result in one completion: validated stored
// state, the gallery entry, the current selection and revision, and the complete
// receipt.
//
// Order, and it is the contract:
//
//  1. Take the profile-domain hold on a pinned connection.
//  2. Read the receipt. A terminal (deleted) receipt is refused with the uniform
//     unavailable answer, whatever the request says. A live receipt's declared
//     shape is checked off the receipt's own columns. Neither step reads a part
//     payload, allocates an id, admits the cap, or charges quota.
//  3. Read the part summary, and hash the payload only where parts are present.
//     A completed key whose parts are gone — the normal retry, whose assembly
//     deleted them — is answered from the receipt alone, with no payload read, no
//     allocation, no cap admission and no charge. A completed key whose parts are
//     still present is measured and compared against the fingerprint the receipt
//     recorded: the receipt is the identity that key bought, not a licence to
//     serve a different request under it. A pending receipt always needs its
//     parts, because it has to write the bytes again.
//  4. Reuse the row the receipt is charged for, under the assembly claim, and
//     allocate a new one only once the row the receipt named is verified absent.
//  5. Complete in one transaction that locks the receipt and the state row
//     before the files-row shared interlock, and takes no owner advisory lock
//     and no state row lock after that file hold.
//
// Locks: the profile-domain session hold and the assembly claim, both on the
// pinned connection, plus inside the transactions the locks the messaging lane
// already takes: the uploader advisory key for the allocation's quota sum, the
// receipt and state row locks, then the files-row FOR SHARE. The uploader key is
// transaction-scoped, released at the allocation commit, and is never held
// across the blob Put. No second owner's lock is taken while this lane holds
// one.
func (s *Store) ProfileUpload(ctx context.Context, req ProfileUploadRequest) (res ProfileUploadResult, err error) {
	if err = req.validate(); err != nil {
		return ProfileUploadResult{}, err
	}
	class, obj, ok := profileOwnerLockKey(req.OwnerID)
	if !ok {
		return ProfileUploadResult{}, fmt.Errorf("profile upload: owner id %d is outside the profile lock domain", req.OwnerID)
	}

	// The lane's own bound, and it is the whole of this lane's claim on assembly
	// capacity: `Open` sized it out of the pool's non-assembly share, so a gallery
	// upload never takes a shared assembly slot and a paused Put in this lane
	// cannot queue another owner's media assembly. A call queued for a token waits
	// in-process, holding no slot and no pooled connection.
	if cap(s.profileAssemblySlots) == 0 {
		// The pool cannot yield the reserve, so this lane declines rather than
		// displace the media assembly capacity every other lane needs.
		return ProfileUploadResult{}, ErrProfileUploadNoCapacity
	}
	select {
	case s.profileAssemblySlots <- struct{}{}:
	case <-ctx.Done():
		return ProfileUploadResult{}, fmt.Errorf("profile upload: wait for the gallery lane's bound: %w", ctx.Err())
	}
	laneReleased := false
	releaseLane := func() {
		if laneReleased {
			return
		}
		laneReleased = true
		<-s.profileAssemblySlots
	}
	defer releaseLane()

	// The whole operation pins one connection: the domain hold and the assembly
	// claim both live on it, and the blob Put runs while it is pinned. The lane
	// bound above is what caps that pinning.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile upload: acquire connection: %w", err)
	}

	locks := profileLocks{
		conn: conn,
		claim: &fileAssemblyClaim{
			conn:        conn,
			scan:        s.assemblyClaimScanHook,
			unlockHook:  s.assemblyClaimUnlockHook,
			discardHook: s.assemblyClaimDiscardHook,
		},
		hold: &profileOwnerHold{class: class, obj: obj},
	}
	// The connection is handed back before the lane token is released, and the
	// lock cleanup runs first inside it: cleanup may wait on a database operation,
	// and no other gallery upload should queue behind that, for the same reason
	// the messaging assembly path releases its slot before its claim cleanup. Both
	// advisory locks live on one session, so exactly one cleanup runs for both.
	defer func() {
		if releaseErr := locks.release(); releaseErr != nil {
			if err == nil {
				res = ProfileUploadResult{}
				err = releaseErr
				return
			}
			err = fmt.Errorf("%w; %w", err, releaseErr)
		}
	}()

	if err = locks.hold.acquire(ctx, conn); err != nil {
		return ProfileUploadResult{}, err
	}

	qc := db.New(conn.Conn())

	// (2) The receipt, before the parts are read.
	rec, found, err := profileReceiptRead(ctx, qc, req.OwnerID, req.ClientFileID)
	if err != nil {
		return ProfileUploadResult{}, err
	}
	if found && rec.State == profileReceiptDeleted {
		// The terminal answer comes first, and nothing in the request can
		// change it. A deleted key reports that the photo is gone; resolving the
		// declared shape first would turn a deleted photo into a fingerprint
		// report, which is a different fact about a key the owner erased.
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	}
	if found {
		// The declared shape is checked off the receipt's own columns, so a
		// retry is answered or rejected before a part is read.
		if rec.MediaMode != profileMediaMode || int(rec.PartCount) != req.Parts {
			return ProfileUploadResult{}, ErrProfileUploadConflict
		}
	}

	// (3) Read the part set. Absent parts (present=false) is the completed key's
	// normal retry state and is answered from the receipt alone; present parts are
	// measured and compared, so a completed key cannot be re-pointed at different
	// bytes, and a pending restart is validated against the fingerprint it stored.
	parts, present, err := s.profileMeasureParts(ctx, conn, req)
	if err != nil {
		return ProfileUploadResult{}, err
	}
	// The fingerprint compare is the dedup contract, and it binds a receipt that
	// has served its identity. A complete receipt names bytes the owner has been
	// shown: a retry naming other bytes under that key is refused. A pending
	// receipt has published nothing, and its pair is a description of parts that
	// are still mutable, so a moved set is not a contract violation: the
	// completion re-fingerprints the pending receipt at the set that exists, which
	// is what keeps a pending key recoverable. The declared shape above binds both
	// states, so a retry cannot re-declare what a key was asked to hold.
	if found && present && rec.State == profileReceiptComplete &&
		(rec.RequestSize != parts.total || !bytes.Equal(rec.PayloadDigest, parts.digest[:])) {
		return ProfileUploadResult{}, ErrProfileUploadConflict
	}
	if found && rec.State == profileReceiptComplete {
		return s.profileCompletedRetry(ctx, qc, req.OwnerID, req.ClientFileID, rec)
	}
	if !present {
		// A pending restart has to re-write the bytes, and a fresh key has
		// nothing to write from.
		return ProfileUploadResult{}, ErrProfilePartsIncomplete
	}

	// Cap admission for the new entry, before an allocation and a charge:
	// a request that cannot add a gallery entry must do nothing at all.
	room, err := profileGalleryHasRoom(ctx, qc, req.OwnerID, req.GalleryCap)
	if err != nil {
		return ProfileUploadResult{}, err
	}
	if !room {
		return ProfileUploadResult{}, ErrProfileGalleryCap
	}

	// (4) The row this key is charged for, then (5) the completion.
	file, err := s.profileChargedRow(ctx, conn, req, rec, found, parts, locks.claim)
	if err != nil {
		return ProfileUploadResult{}, err
	}
	res, err = s.profileComplete(ctx, conn, req, file, parts)
	if errors.Is(err, errProfileRowAgedOut) {
		// The only way a charge moves to a new row: the row the receipt named is
		// gone, verified by the reference interlock itself, and with it its
		// charge. Drop the claim on the row that is not coming back, allocate
		// anew, and complete once more under the same hold.
		if releaseErr := locks.claim.unlockCurrent(ctx); releaseErr != nil {
			return ProfileUploadResult{}, releaseErr
		}
		file, err = s.profileAllocate(ctx, conn, req, parts, locks.claim, true)
		if err != nil {
			return ProfileUploadResult{}, err
		}
		res, err = s.profileComplete(ctx, conn, req, file, parts)
	}
	return res, err
}

// profileReceiptRead is the dedup lookup. pgx.ErrNoRows is the absent key, not
// an error.
func profileReceiptRead(ctx context.Context, qc *db.Queries, ownerID, clientFileID int64) (db.ProfileUploadReceipt, bool, error) {
	rec, err := qc.ProfileUploadReceipt(ctx, db.ProfileUploadReceiptParams{
		UserID:       ownerID,
		ClientFileID: clientFileID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ProfileUploadReceipt{}, false, nil
	}
	if err != nil {
		return db.ProfileUploadReceipt{}, false, fmt.Errorf("profile upload receipt: %w", err)
	}
	return rec, true, nil
}

// profileCompletedRetry answers a completed key from the rows its receipt names,
// and those rows are read: the gallery row, the file row, the state row. What is
// never written is the point — no allocation, no blob write, no gallery entry, no
// selection change, no charge. What is never read here is a part payload: where
// parts are still present the caller measured them before the call, which is the
// one part read a replay performs, and the parts-absent case reads no payload at
// all. The gallery row is the proof the photo is live, and a complete receipt
// without one is a state this lane refuses to serve, because re-assembling it
// would resurrect a photo its owner deleted.
func (s *Store) profileCompletedRetry(
	ctx context.Context, qc *db.Queries, ownerID, clientFileID int64, rec db.ProfileUploadReceipt,
) (ProfileUploadResult, error) {
	if rec.FileID == nil {
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	}
	photo, err := qc.UserPhotoByClientFileID(ctx, db.UserPhotoByClientFileIDParams{
		UserID:       ownerID,
		ClientFileID: clientFileID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	}
	if err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile gallery entry: %w", err)
	}
	if photo.FileID != *rec.FileID {
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	}
	row, err := qc.ProfileFileForOwner(ctx, db.ProfileFileForOwnerParams{
		ID:         photo.FileID,
		UploaderID: ownerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	}
	if err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile file row: %w", err)
	}
	file := profileFileFromRow(row)
	if !file.Stored {
		// A complete receipt whose bytes are not stored is not servable, and
		// re-running the assembly would complete a receipt that says it is done.
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	}
	var revision int64
	state, err := qc.GetProfilePhotoState(ctx, ownerID)
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return ProfileUploadResult{}, fmt.Errorf("profile photo state: %w", err)
	case err == nil:
		revision = state.MutationRevision
	}
	return ProfileUploadResult{
		File:             file,
		ClientFileID:     clientFileID,
		MutationRevision: revision,
		Replayed:         true,
	}, nil
}

// profilePartSnapshot reads the declared part set from one snapshot of
// upload_parts: the refs in index order, their summed size, and whether a set is
// there at all. The set must be exactly the contiguous indexes the request
// declares, each with bytes: a gap is a client that has not finished, not a
// partial file to charge for. Count, total and the cap all come out of the one
// result set, so they cannot be read from two instants.
func profilePartSnapshot(ctx context.Context, q *db.Queries, req ProfileUploadRequest) (profileParts, bool, error) {
	refs, err := q.UploadPartRefs(ctx, db.UploadPartRefsParams{
		UserID: req.OwnerID,
		FileID: req.ClientFileID,
	})
	if err != nil {
		return profileParts{}, false, fmt.Errorf("profile upload part refs: %w", err)
	}
	if len(refs) == 0 {
		return profileParts{}, false, nil
	}
	parts := profileParts{refs: make([]UploadPartRef, len(refs))}
	for i, r := range refs {
		// The snapshot is the declared set or it is a gap: a short set, a long
		// set and a hole in the indexes are all a client that has not finished.
		if int(r.PartIndex) != i || len(refs) != req.Parts || r.Size <= 0 {
			return profileParts{}, false, ErrProfilePartsIncomplete
		}
		parts.refs[i] = UploadPartRef{Index: int(r.PartIndex), Key: r.BlobKey, Size: r.Size}
		parts.total += r.Size
	}
	if parts.total <= 0 || parts.total > req.MaxFileBytes {
		return profileParts{}, false, ErrProfilePartsIncomplete
	}
	return parts, true, nil
}

// profileDigestParts hashes the payload behind a snapshot's refs. It runs outside
// the snapshot, over exactly the keys it named: a part save that moved a row
// deletes the object its row left, and ReadUploadPart fails closed on the missing
// object and on a short read. The digest is therefore a function of the snapshot,
// which is what makes a receipt's size and digest pair reproducible.
func (s *Store) profileDigestParts(ctx context.Context, parts profileParts) ([]byte, error) {
	hash := sha256.New()
	for _, ref := range parts.refs {
		payload, err := s.ReadUploadPart(ctx, ref)
		if err != nil {
			return nil, err
		}
		if _, err := hash.Write(payload); err != nil {
			return nil, fmt.Errorf("profile upload digest: %w", err)
		}
	}
	return hash.Sum(nil), nil
}

// profileMeasureParts takes the parts snapshot on the pinned connection in its own
// read-only repeatable-read transaction, then digests it, and reports whether a
// set is there at all. No parts is a distinct answer, because a completed key's
// assembly deleted them and its retry must be served from the receipt.
//
// A key's parts stay mutable while it uploads: a concurrent part save replaces a
// row's size, moves the row to a new object and deletes the superseded object
// after committing. One snapshot is what keeps the receipt's request_size and
// payload_digest one claim about one content: a size from one instant and a
// digest from another names a pair no measurement can reproduce, and the key's
// coherent retry would be rejected for the rest of the key's life.
func (s *Store) profileMeasureParts(ctx context.Context, conn *pgxpool.Conn, req ProfileUploadRequest) (profileParts, bool, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return profileParts{}, false, fmt.Errorf("profile upload parts snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	parts, present, err := profilePartSnapshot(ctx, db.New(tx), req)
	if err != nil {
		return profileParts{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return profileParts{}, false, fmt.Errorf("profile upload parts snapshot: %w", err)
	}
	if !present {
		return profileParts{}, false, nil
	}
	if s.profilePartsSnapshotHook != nil {
		s.profilePartsSnapshotHook(req.OwnerID, req.ClientFileID)
	}
	digest, err := s.profileDigestParts(ctx, parts)
	if err != nil {
		return profileParts{}, false, err
	}
	copy(parts.digest[:], digest)
	return parts, true, nil
}

// profileReconcileParts re-measures the part set at the last point before the
// bytes are written, and reconciles it with the receipt that was just locked.
//
// The parts are mutable until the assembly consumes them, and a save deletes the
// object the row it replaced pointed at, so the set digested earlier in this
// upload can stop being readable between the digest and the Put. Reading the
// snapshot inside this transaction is the freshest set available, and it decides
// three ways:
//
//   - the set still matches the receipt, so the assembly is pointed at these
//     refs, which name the content the receipt recorded;
//   - the set has moved and the receipt is pending, so the receipt is
//     re-fingerprinted at it. Nothing has been published for this key and its
//     charge is the same row, and the alternative is a pending key that no
//     measurement can ever serve, which is a dead key and a dead charge;
//   - the set has moved and the receipt is complete, which is the dedup contract
//     refusing to re-point an identity the key already bought.
//
// A replacement that lands inside this reconciliation fails the digest
// read closed, leaving the receipt pending, and the next restart reconciles the
// set that exists then. The invariant is that a pending key is always recoverable
// by a coherent retry.
func (s *Store) profileReconcileParts(
	ctx context.Context, qtx *db.Queries, req ProfileUploadRequest, rec db.ProfileUploadReceipt,
	parts profileParts,
) (profileParts, error) {
	cur, present, err := profilePartSnapshot(ctx, qtx, req)
	if err != nil {
		return profileParts{}, err
	}
	if !present {
		// No parts at all: the set this upload measured is gone and there is
		// nothing to write. Leave the recorded pair alone; the eraser's reclaim of
		// the row is what the aged-out path in the charged-row step resolves.
		return parts, nil
	}
	digest, err := s.profileDigestParts(ctx, cur)
	if err != nil {
		return profileParts{}, err
	}
	if cur.total == rec.RequestSize && bytes.Equal(digest, rec.PayloadDigest) {
		return cur, nil
	}
	if rec.State != profileReceiptPending {
		return profileParts{}, ErrProfileUploadConflict
	}
	if len(cur.refs) > 1<<31-1 {
		// The column is int32; a set this large is not a photo, and it is
		// refused rather than narrowed.
		return profileParts{}, ErrProfilePartsIncomplete
	}
	rows, err := qtx.SetProfileUploadReceiptFingerprint(ctx, db.SetProfileUploadReceiptFingerprintParams{
		UserID:        req.OwnerID,
		ClientFileID:  req.ClientFileID,
		RequestSize:   cur.total,
		PartCount:     int32(len(cur.refs)), //nolint:gosec // G115: bounded by the int32 ceiling check above
		PayloadDigest: digest,
	})
	if err != nil {
		return profileParts{}, fmt.Errorf("profile upload receipt fingerprint: %w", err)
	}
	if rows == 0 {
		// The receipt moved out of pending under this transaction, which
		// locked it, so the pair it recorded is not ours to change.
		return profileParts{}, ErrProfileUploadConflict
	}
	cur.digest = [sha256.Size]byte{}
	copy(cur.digest[:], digest)
	return cur, nil
}

// profileGalleryHasRoom is the cap admission for a new entry. It runs before any
// allocation, so a request that cannot add an entry does no work at all, and the
// completion re-checks it under its own row locks. It is always after the receipt
// lookup, which is what lets a completed key answer under a full gallery.
func profileGalleryHasRoom(ctx context.Context, q *db.Queries, ownerID int64, galleryCap int) (bool, error) {
	if galleryCap <= 0 {
		return true, nil
	}
	entries, err := q.CountUserPhotos(ctx, ownerID)
	if err != nil {
		return false, fmt.Errorf("profile gallery count: %w", err)
	}
	return entries < int64(galleryCap), nil
}

// profileChargedRow returns the file row this key is charged for and claims it
// for this assembly. A fresh key allocates. A pending receipt reuses the row it
// already charged.
//
// The row's absence is not decided by a read racing a reclaimer: for a pending
// receipt the completion's own reference interlock is what proves it, which is
// what makes "allocate anew only after confirming the old row is absent" the
// observable rule. A null pointer and a row already gone at claim time are the
// two cases that can be settled up front, and both mean the charge the receipt
// carried has been retired.
func (s *Store) profileChargedRow(
	ctx context.Context, conn *pgxpool.Conn, req ProfileUploadRequest,
	rec db.ProfileUploadReceipt, found bool, parts profileParts, claim *fileAssemblyClaim,
) (File, error) {
	if !found {
		return s.profileAllocate(ctx, conn, req, parts, claim, false)
	}
	if rec.FileID == nil {
		// The pointer is null because the bytes were reclaimed and the
		// receipt's foreign key nulled it: the row the charge named is gone.
		return s.profileAllocate(ctx, conn, req, parts, claim, true)
	}
	fileID := *rec.FileID
	acquired, err := claim.tryAcquire(ctx, fileID)
	if err != nil {
		return File{}, err
	}
	if !acquired {
		return File{}, ErrProfileAssemblyClaimed
	}
	row, err := db.New(conn.Conn()).ProfileFileForOwner(ctx, db.ProfileFileForOwnerParams{
		ID:         fileID,
		UploaderID: req.OwnerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Aged out with the pointer not yet null: the same verified-absent path,
		// and the claim on the vanished row goes with it.
		if releaseErr := claim.unlockCurrent(ctx); releaseErr != nil {
			return File{}, releaseErr
		}
		return s.profileAllocate(ctx, conn, req, parts, claim, true)
	}
	if err != nil {
		return File{}, fmt.Errorf("profile file row: %w", err)
	}
	return profileFileFromRow(row), nil
}

// profileAllocate reserves the row and records the pending receipt in the same
// transaction, so the charge and the durable retry identity become visible
// together. The claim is taken before the commit, exactly as the messaging
// assembly path takes it, and the uploader advisory key is taken inside this
// transaction for the quota sum and released by its commit.
func (s *Store) profileAllocate(
	ctx context.Context, conn *pgxpool.Conn, req ProfileUploadRequest,
	parts profileParts, claim *fileAssemblyClaim, repoint bool,
) (File, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return File{}, fmt.Errorf("profile upload: begin allocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	qtx := s.q.WithTx(tx)
	file, err := allocateFileTx(ctx, tx, qtx, req.OwnerID, parts.total, req.MimeType, req.FileName, req.MaxUserBytes, nil)
	if err != nil {
		return File{}, err
	}
	acquired, err := claim.tryAcquire(ctx, file.ID)
	if err != nil {
		return File{}, err
	}
	if !acquired {
		return File{}, ErrProfileAssemblyClaimed
	}
	if repoint {
		// The receipt exists and its charge is being moved onto a row that
		// exists. Its fingerprint is immutable: only the pointer moves.
		if _, err := qtx.SetProfileUploadReceiptFile(ctx, db.SetProfileUploadReceiptFileParams{
			UserID:       req.OwnerID,
			ClientFileID: req.ClientFileID,
			FileID:       &file.ID,
		}); err != nil {
			return File{}, fmt.Errorf("profile upload receipt re-point: %w", err)
		}
	} else {
		// The measured set is exactly the declared part count, whose top index the
		// summary reported as an int32, so the count fits the column that
		// stores it.
		partCount := int32(len(parts.refs)) //nolint:gosec // G115: bounded by the int32 part_index the summary checked
		if _, err := qtx.InsertProfileUploadReceipt(ctx, db.InsertProfileUploadReceiptParams{
			UserID:        req.OwnerID,
			ClientFileID:  req.ClientFileID,
			FileID:        &file.ID,
			RequestSize:   parts.total,
			PartCount:     partCount,
			PayloadDigest: parts.digest[:],
		}); err != nil {
			return File{}, fmt.Errorf("profile upload receipt: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return File{}, fmt.Errorf("profile upload: commit allocation: %w", err)
	}
	return file, nil
}

// profileComplete publishes the whole result in one transaction: the receipt and
// the state row are locked, then the files row's shared reference interlock, then
// the blob write runs under that interlock, then stored state, gallery entry,
// current selection and the complete receipt are written together.
//
// The order is the one the design fixes: profile receipt and state before the
// terminal file-reference interlock, and no owner advisory lock and no state row
// lock after that file hold. The eraser takes the files row with SKIP LOCKED and
// only probes the assembly claim without waiting, so it cannot form a cycle with
// this transaction.
//
// A failed assembly, an invalid photo, a cap refusal, or any failure after the
// blob write rolls this back: the row stays unstored, the receipt stays pending,
// and no gallery entry exists, so an incomplete file can never become visible.
func (s *Store) profileComplete(
	ctx context.Context, conn *pgxpool.Conn, req ProfileUploadRequest,
	file File, parts profileParts,
) (ProfileUploadResult, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile upload: begin completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	qtx := s.q.WithTx(tx)

	rec, err := qtx.LockProfileUploadReceipt(ctx, db.LockProfileUploadReceiptParams{
		UserID:       req.OwnerID,
		ClientFileID: req.ClientFileID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key's receipt is gone, which means its owner's account is gone,
		// and the gallery entry this would have made has no owner to be in.
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	case err != nil:
		return ProfileUploadResult{}, fmt.Errorf("profile upload receipt lock: %w", err)
	}
	if rec.State == profileReceiptDeleted {
		return ProfileUploadResult{}, ErrProfilePhotoUnavailable
	}

	if err = qtx.EnsureProfilePhotoState(ctx, req.OwnerID); err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile photo state: %w", err)
	}
	if _, err = qtx.LockProfilePhotoState(ctx, req.OwnerID); err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile photo state lock: %w", err)
	}

	// The terminal file-reference interlock, last of the three, and taken before
	// the blob write as every assembly in this server takes it. Its missing row
	// is the verified absence that permits a new allocation.
	if err = lockFileRefs(ctx, qtx, file.ID); err != nil {
		if errors.Is(err, ErrFileMissing) {
			return ProfileUploadResult{}, errProfileRowAgedOut
		}
		return ProfileUploadResult{}, err
	}

	// The parts are mutable until the assembly consumes them, so the set is
	// re-measured here, inside this transaction and under the file interlock, and
	// the receipt is reconciled with it before a byte is written.
	parts, err = s.profileReconcileParts(ctx, qtx, req, rec, parts)
	if err != nil {
		return ProfileUploadResult{}, err
	}
	// The content moved, so the charge moves with it. files.size is the
	// lifetime quota sum, so the row has to describe the bytes this transaction
	// stores; the delta is re-admitted in the same subtraction form the allocation
	// uses, and the size is only ever moved on a row this lane allocated and has
	// not stored.
	if parts.total != file.Size {
		if parts.total > file.Size {
			used, err := qtx.UserStoredBytes(ctx, req.OwnerID)
			if err != nil {
				return ProfileUploadResult{}, fmt.Errorf("profile upload quota: %w", err)
			}
			grew := parts.total - file.Size
			if grew > req.MaxUserBytes || used > req.MaxUserBytes-grew {
				return ProfileUploadResult{}, ErrStorageQuota
			}
		}
		rows, err := qtx.SetProfileFileSize(ctx, db.SetProfileFileSizeParams{
			ID:         file.ID,
			UploaderID: req.OwnerID,
			Size:       parts.total,
		})
		if err != nil {
			return ProfileUploadResult{}, fmt.Errorf("profile upload charge size: %w", err)
		}
		if rows == 0 {
			// The row is gone or already stored: neither is a row whose charge
			// this transaction may move.
			return ProfileUploadResult{}, ErrFileMissing
		}
		file.Size = parts.total
	}

	if s.profilePartsBeforeAssembleHook != nil {
		s.profilePartsBeforeAssembleHook(req.OwnerID, req.ClientFileID)
	}

	dimensions, err := req.Assemble(ProfileAssembly{File: file, Parts: parts.refs, Total: parts.total})
	if err != nil {
		return ProfileUploadResult{}, err
	}
	if !validPhotoDimensions(dimensions) {
		return ProfileUploadResult{}, ErrInvalidPhotoDimensions
	}

	// Stored state, the validated dimensions and the subtype rights travel in one
	// statement, so no reader can see a stored photo whose dimensions were not
	// the ones that were validated.
	rows, err := qtx.MarkPhotoFileStored(ctx, db.MarkPhotoFileStoredParams{
		ID:     file.ID,
		Width:  &dimensions.Width,
		Height: &dimensions.Height,
	})
	if err != nil {
		return ProfileUploadResult{}, fmt.Errorf("mark profile file stored: %w", err)
	}
	if rows == 0 {
		return ProfileUploadResult{}, ErrFileMissing
	}

	// Cap admission for the new entry, re-checked under this transaction's locks:
	// the count read above is outside them, and this is the insert the cap bounds.
	room, err := profileGalleryHasRoom(ctx, qtx, req.OwnerID, req.GalleryCap)
	if err != nil {
		return ProfileUploadResult{}, err
	}
	if !room {
		return ProfileUploadResult{}, ErrProfileGalleryCap
	}
	if _, err = qtx.InsertUserPhoto(ctx, db.InsertUserPhotoParams{
		UserID:       req.OwnerID,
		FileID:       file.ID,
		ClientFileID: req.ClientFileID,
	}); err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile gallery entry: %w", err)
	}

	state, err := qtx.SelectProfilePhoto(ctx, db.SelectProfilePhotoParams{
		UserID:        req.OwnerID,
		CurrentFileID: &file.ID,
	})
	if err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile current selection: %w", err)
	}

	completed, err := qtx.CompleteProfileUploadReceipt(ctx, db.CompleteProfileUploadReceiptParams{
		UserID:       req.OwnerID,
		ClientFileID: req.ClientFileID,
		FileID:       &file.ID,
	})
	if err != nil {
		return ProfileUploadResult{}, fmt.Errorf("complete profile upload receipt: %w", err)
	}
	if completed == 0 {
		// The receipt was not pending while this transaction held its lock, so
		// the transition this completion publishes already happened.
		return ProfileUploadResult{}, ErrProfileUploadConflict
	}

	if err = tx.Commit(ctx); err != nil {
		return ProfileUploadResult{}, fmt.Errorf("profile upload: commit completion: %w", err)
	}

	file.Stored = true
	file.Kind = FileKindPhoto
	file.Width = int(dimensions.Width)
	file.Height = int(dimensions.Height)
	file.SubtypeRights = []string{"send_photos"}
	return ProfileUploadResult{
		File:             file,
		ClientFileID:     req.ClientFileID,
		MutationRevision: state.MutationRevision,
	}, nil
}

// profileFileFromRow maps an owner-scoped files row onto File. It is the read a
// completed receipt answers with, so it reports the identity that was stored,
// including the photo dimensions validated at completion.
func profileFileFromRow(r db.ProfileFileForOwnerRow) File {
	var width, height int
	if r.Width != nil {
		width = int(*r.Width)
	}
	if r.Height != nil {
		height = int(*r.Height)
	}
	return File{
		ID:            r.ID,
		UploaderID:    r.UploaderID,
		AccessHash:    r.AccessHash,
		Size:          r.Size,
		MimeType:      r.MimeType,
		FileName:      r.FileName,
		Kind:          FileKind(r.MediaKind),
		Width:         width,
		Height:        height,
		SubtypeRights: r.SubtypeRights,
		Stored:        r.Stored,
		Date:          r.Date.Time,
	}
}

// profileOwnerHold is the gallery lane's serialization hold: a session-scoped
// advisory lock in the two-argument profile domain, taken on the same pinned
// connection that carries the assembly claim.
//
// Session scope is deliberate, and the reasoning is the assembly claim's: the
// allocation transaction commits and the blob Put runs afterwards, so a
// transaction-scoped lock would fall off in the middle of the operation. A
// crashed process loses the connection and with it the hold, and a waiting
// duplicate then finds the receipt its predecessor left.
type profileOwnerHold struct {
	class int32
	obj   int32
	held  bool
	// discard records that the acquisition's result was not consumed. A session
	// advisory lock survives a query cancellation, so this session may be holding
	// the lock, and the release path closes it rather than pooling it.
	discard bool
}

// acquire takes the hold, waiting for a duplicate's in-flight upload.
func (h *profileOwnerHold) acquire(ctx context.Context, conn *pgxpool.Conn) error {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1::int, $2::int)`, h.class, h.obj); err != nil {
		h.discard = true
		return fmt.Errorf("profile serialization hold: acquire %d/%d: %w", h.class, h.obj, err)
	}
	h.held = true
	return nil
}

// unlock releases the hold on the session that took it. It is the release path's
// only use of the connection, so the lock cleanup stays in one place.
func (h *profileOwnerHold) unlock(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var released bool
	err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1::int, $2::int)`, h.class, h.obj).Scan(&released)
	return released, err
}

// profileLocks owns the one pinned connection the gallery lane holds for a whole
// upload and the two session advisory locks that live on it: the assembly claim
// and the profile-domain hold.
//
// One cleanup for both is not a convenience. Releasing a session lock can require
// closing the session, and closing is a one-shot hijack of the pooled
// connection: two independent cleanups on one connection is a panic path.
type profileLocks struct {
	conn  *pgxpool.Conn
	claim *fileAssemblyClaim
	hold  *profileOwnerHold
}

type profileUnlockOutcome struct {
	claimReleased bool
	holdReleased  bool
	err           error
}

// release unlocks the claim, then the domain hold, then hands the connection
// back.
//
// A claim or hold marked for discard closes the session, and an unlock whose
// query fails closes it too: the server releases every advisory lock that
// session held when the connection closes, which is what keeps a lock from
// travelling to unrelated work.
//
// An unlock whose query ran and reported the lock as not held is a different
// fact: this session does not hold that advisory lock, so there is nothing left
// on the connection to outlive the request, and the connection goes back to
// the pool. The error it returns says so, because a lock this lane expected
// to own is a fact worth reporting even when the cleanup is safe.
//
// The unlock runs under the assembly claim's own one-second bound, and the
// operation is joined before the connection is hijacked, so a Close can never
// race a query on the same connection. That is the same discipline
// fileAssemblyClaim.release applies on its own.
func (l *profileLocks) release() error {
	conn := l.conn
	if conn == nil {
		return nil
	}
	l.conn = nil
	claim, hold := l.claim, l.hold

	if claim.discard || hold.discard {
		if err := discardFileAssemblyConnection(conn); err != nil {
			return fmt.Errorf("profile upload locks: discard: %w", err)
		}
		return nil
	}
	if !claim.held && !hold.held {
		conn.Release()
		return nil
	}

	unlockCtx, cancel := context.WithTimeout(context.Background(), assemblyClaimUnlockTimeout)
	defer cancel()
	outcome := make(chan profileUnlockOutcome, 1)
	rawConn := conn.Conn()
	go func() {
		res := profileUnlockOutcome{}
		defer func() { outcome <- res }()
		if claim.held {
			res.claimReleased, res.err = claim.unlock(unlockCtx, rawConn)
			if res.err != nil {
				return
			}
		} else {
			res.claimReleased = true
		}
		if hold.held {
			res.holdReleased, res.err = hold.unlock(unlockCtx, rawConn)
		} else {
			res.holdReleased = true
		}
	}()

	select {
	case res := <-outcome:
		if res.err != nil {
			if closeErr := discardFileAssemblyConnection(conn); closeErr != nil {
				return fmt.Errorf("profile upload locks: unlock: %w; close: %w", res.err, closeErr)
			}
			return nil
		}
		if !res.claimReleased || !res.holdReleased {
			conn.Release()
			return errors.New("profile upload locks: a session advisory lock was not held")
		}
		conn.Release()
		return nil
	case <-unlockCtx.Done():
		// Join the unlock before hijacking: pgx cancellation and this select race
		// at the deadline, and Close must not run concurrently with a query.
		<-outcome
		if closeErr := discardFileAssemblyConnection(conn); closeErr != nil {
			return fmt.Errorf("profile upload locks: unlock timed out: %w; close: %w", unlockCtx.Err(), closeErr)
		}
		return nil
	}
}
