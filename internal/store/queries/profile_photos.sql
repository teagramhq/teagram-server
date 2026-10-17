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

-- ProfilePhotosByOwner pages the owner's published gallery. A viewer who can
-- resolve the owner through a valid InputUser may list stored gallery entries
-- unless the owner blocked that viewer. This directed block edge matches the
-- download gate; the reverse edge does not hide the owner's photos.
-- offset=-1 is Telegram's file-reference refresh form: max_id names the exact
-- photo to return. Ordinary max_id pages use ids strictly below the cursor.
-- visible_count always returns one row, including when the page is empty, so an
-- out-of-range cursor still reports the visible gallery size. The left-join
-- sentinel has has_photo=false and carries no file identity.
-- name: ProfilePhotosByOwner :many
WITH visible AS MATERIALIZED (
    SELECT f.id, f.uploader_id, f.access_hash, f.size, f.mime_type, f.file_name,
           f.stored, f.date, f.subtype_rights, f.media_kind, f.width, f.height,
           d.file_id AS derivative_file_id,
           d.m_width, d.m_height, d.m_size, d.stripped
    FROM user_photos up
    JOIN users u ON u.id = up.user_id
    JOIN files f ON f.id = up.file_id AND f.uploader_id = up.user_id
    LEFT JOIN photo_derivatives d ON d.file_id = f.id
    WHERE up.user_id = sqlc.arg(owner_id)::bigint
      AND f.stored = true
      AND f.media_kind = 'photo'
      AND NOT EXISTS (
          SELECT 1 FROM blocked_users bu
          WHERE bu.blocker_id = up.user_id
            AND bu.blocked_id = sqlc.arg(viewer_id)::bigint
      )
), page AS (
    SELECT *
    FROM visible
    WHERE (sqlc.arg(page_offset)::bigint = -1 AND id = sqlc.arg(max_id)::bigint)
       OR (sqlc.arg(page_offset)::bigint >= 0 AND (sqlc.arg(max_id)::bigint <= 0 OR id < sqlc.arg(max_id)::bigint))
    ORDER BY id DESC
    LIMIT sqlc.arg(page_limit)::bigint
    OFFSET GREATEST(sqlc.arg(page_offset)::bigint, 0)
), visible_count AS (
    SELECT count(*)::bigint AS total FROM visible
)
SELECT COALESCE(page.id, 0)::bigint > 0 AS has_photo,
       COALESCE(page.id, 0)::bigint AS id,
       COALESCE(page.uploader_id, 0)::bigint AS uploader_id,
       COALESCE(page.access_hash, 0)::bigint AS access_hash,
       COALESCE(page.size, 0)::bigint AS size,
       COALESCE(page.mime_type, '')::text AS mime_type,
       COALESCE(page.file_name, '')::text AS file_name,
       COALESCE(page.stored, false)::boolean AS stored,
       COALESCE(page.date, 'epoch'::timestamptz) AS date,
       page.subtype_rights,
       COALESCE(page.media_kind, '')::text AS media_kind,
       page.width, page.height,
       page.derivative_file_id, page.m_width, page.m_height, page.m_size,
       page.stripped, visible_count.total
FROM visible_count
LEFT JOIN page ON true
ORDER BY page.id DESC;

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
