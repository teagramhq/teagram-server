-- The gallery lane's own queries: the retry receipt, the gallery entry, and the
-- current selection. Nothing here deletes a receipt, a gallery entry, or a
-- state row. The receipt is the durable retry identity, so its whole life is
-- insert, re-point, and terminalize by a later writer; compaction is a separate
-- bounded decision (MAIN-1446), not a side effect of an upload.

-- ProfileUploadReceipt is the upload dedup lookup, and it runs before anything
-- the answer can make redundant: before the upload parts are read, before a
-- file id is allocated, before the gallery cap is admitted, and before quota is
-- charged. A completion-time uniqueness conflict cannot buy that, because the
-- allocation commits the files row before the bytes exist and the quota sums
-- every row, stored or not.
--
-- file_id is read here as a nullable pointer, not as a live media reference:
-- the receipt's file foreign key is ON DELETE SET NULL, so reclamation of the
-- bytes keeps the owner's retry record and clears the pointer alone.
-- name: ProfileUploadReceipt :one
SELECT user_id, client_file_id, file_id, state, request_size, part_count,
       payload_digest, media_mode, created_at, updated_at
FROM profile_upload_receipt
WHERE user_id = $1 AND client_file_id = $2;

-- LockProfileUploadReceipt is the receipt half of the completion ordering: the
-- receipt and the state row are locked before the files row's shared reference
-- interlock, and nothing takes an owner advisory lock or a state row lock after
-- that file hold. Locking the receipt is what makes a concurrent writer of the
-- same key queue behind this completion rather than race it.
-- name: LockProfileUploadReceipt :one
SELECT user_id, client_file_id, file_id, state, request_size, part_count,
       payload_digest, media_mode, created_at, updated_at
FROM profile_upload_receipt
WHERE user_id = $1 AND client_file_id = $2
FOR UPDATE;

-- InsertProfileUploadReceipt records the pending receipt with the file id the
-- same transaction allocated, so the charge and the retry identity become
-- visible together. state 0 is pending: an assembly that dies here leaves a
-- claim the eraser can retire, and a retry that finds it reuses that row.
--
-- Every fingerprint column is server-measured. request_size is the sum of the
-- recorded part sizes, part_count is the measured part set, and
-- payload_digest is SHA-256 over the assembled part bytes. Nothing on the
-- request is trusted for the fingerprint, which is what lets a retry with the
-- same key be compared against the bytes the key already bought. media_mode is a
-- literal: the gallery lane has one mode, and an unsupported request never
-- reaches this statement, so a receipt can only ever name the mode its owner
-- can re-present.
-- name: InsertProfileUploadReceipt :one
INSERT INTO profile_upload_receipt (
    user_id, client_file_id, file_id, state,
    request_size, part_count, payload_digest, media_mode
)
VALUES ($1, $2, $3, 0, $4, $5, $6, 'photo')
RETURNING user_id, client_file_id, file_id, state, request_size, part_count,
          payload_digest, media_mode, created_at, updated_at;

-- SetProfileUploadReceiptFile re-points a pending receipt at a new allocation.
-- It is reached only after the row the receipt named was verified absent, so
-- the charge moves from a row the eraser already retired to a fresh one; state
-- 0 in the predicate means a completed receipt can never be re-pointed.
-- name: SetProfileUploadReceiptFile :execrows
UPDATE profile_upload_receipt
SET file_id = $3, updated_at = now()
WHERE user_id = $1 AND client_file_id = $2 AND state = 0 AND file_id IS DISTINCT FROM $3;

-- CompleteProfileUploadReceipt is the receipt half of the completion commit:
-- state 1 names the file whose bytes, gallery entry and current selection are
-- made visible by the same transaction. The state 0 predicate is what makes
-- completion a one-time transition: a second completion of the same key moves
-- zero rows and fails the assembly closed instead of re-charging it.
-- name: CompleteProfileUploadReceipt :execrows
UPDATE profile_upload_receipt
SET state = 1, file_id = $3, updated_at = now()
WHERE user_id = $1 AND client_file_id = $2 AND state = 0;

-- InsertUserPhoto inserts the gallery entry. The composite foreign key to
-- files (id, uploader_id) is the structural ownership backstop: an entry
-- naming a file this owner does not own cannot be stored, and RESTRICT is what
-- keeps an eraser from removing the entry under a photo the account is showing.
-- client_file_id carries the dedup key the UNIQUE (user_id, client_file_id)
-- constraint enforces, one client id to one entry per owner.
-- name: InsertUserPhoto :one
INSERT INTO user_photos (user_id, file_id, client_file_id)
VALUES ($1, $2, $3)
RETURNING user_id, file_id, client_file_id, created_at;

-- UserPhotoByClientFileID resolves an owner's entry by its upload identity.
-- This is the read that makes a completed retry return the identity the key
-- already bought, with no parts, no allocation and no new charge.
-- name: UserPhotoByClientFileID :one
SELECT user_id, file_id, client_file_id, created_at
FROM user_photos
WHERE user_id = $1 AND client_file_id = $2;

-- CountUserPhotos is the gallery cap's input, and it is admission for a new
-- entry: the cap is consulted after the receipt lookup, so a completed retry
-- under a full gallery is never refused for a limit it does not exercise.
-- name: CountUserPhotos :one
SELECT count(*)::bigint AS entries FROM user_photos WHERE user_id = $1;

-- EnsureProfilePhotoState creates the per-owner selection row so the lock below
-- always has a row to take. current_file_id is NULL, which a MATCH SIMPLE
-- composite foreign key leaves unchecked: that is the empty-avatar state. The
-- row is created at 0 and is never compacted, so an acknowledged clear survives
-- a restore that predates the upload it cleared.
-- name: EnsureProfilePhotoState :exec
INSERT INTO profile_photo_state (user_id, current_file_id, mutation_revision)
VALUES ($1, NULL, 0)
ON CONFLICT (user_id) DO NOTHING;

-- LockProfilePhotoState is the state half of the completion ordering, taken
-- with the receipt and before the files interlock.
-- name: LockProfilePhotoState :one
SELECT user_id, current_file_id, mutation_revision
FROM profile_photo_state
WHERE user_id = $1
FOR UPDATE;

-- GetProfilePhotoState reads the current selection without locking it, for
-- reporting an upload that was answered from a completed receipt.
-- name: GetProfilePhotoState :one
SELECT user_id, current_file_id, mutation_revision
FROM profile_photo_state
WHERE user_id = $1;

-- SelectProfilePhoto makes the just-inserted gallery entry current and advances
-- the per-owner revision. It runs after the state row is locked in this
-- transaction, and the composite foreign key is what proves the pointer names
-- a live entry in this owner's own gallery.
-- name: SelectProfilePhoto :one
UPDATE profile_photo_state
SET current_file_id = $2, mutation_revision = mutation_revision + 1
WHERE user_id = $1
RETURNING user_id, current_file_id, mutation_revision;

-- ProfileFileForOwner loads one of an owner's own file rows, stored or not, for
-- the identity a completed receipt returns. Owner-scoped on purpose:
-- the gallery lane never resolves a file id that its owner does not
-- own, and this is the read that makes that true rather than a convention.
-- name: ProfileFileForOwner :one
SELECT id, uploader_id, access_hash, size, mime_type, file_name, media_kind,
       width, height, subtype_rights, stored, date
FROM files
WHERE id = $1 AND uploader_id = $2;
