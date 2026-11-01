-- Sticker-set owners are locked for the count-and-create and install-limit
-- transactions. This lock is acquired before any file-reference lock.
-- name: LockUserStickerSetOwner :one
SELECT id FROM users WHERE id = $1 FOR UPDATE;

-- name: UserCreatedStickerSetCount :one
SELECT count(*)::bigint
FROM user_created_sticker_sets
WHERE creator_id = $1;

-- name: CreateUserCreatedStickerSet :one
INSERT INTO user_created_sticker_sets
    (access_hash, creator_id, title, short_name, software, kind, text_color, item_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, access_hash, creator_id, title, short_name, software, kind,
          text_color, item_count, set_hash, is_live, created_at;

-- The hash includes the generated set ID and every inserted item, so update it
-- before the enclosing creation transaction commits.
-- name: UpdateUserCreatedStickerSetHash :execrows
UPDATE user_created_sticker_sets
SET set_hash = $2
WHERE id = $1;

-- name: InsertUserCreatedStickerSetItem :execrows
INSERT INTO user_created_sticker_set_items
    (set_id, position, file_id, emoji, keywords, format, width, height, duration_ns, mask_coords, content_sha256)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (file_id) DO NOTHING;

-- Sticker inputs are resolved together through caller-owned, stored document
-- rows and exact access hashes. Files already assigned to any set are refused
-- before the API reads any blob bytes.
-- name: StickerFilesForOwner :many
SELECT id, uploader_id, access_hash, size, mime_type, file_name, stored, date,
       subtype_rights, media_kind, width, height
FROM unnest(sqlc.arg(file_ids)::bigint[])
     WITH ORDINALITY AS requested(file_id, request_order)
JOIN files ON files.id = requested.file_id
          AND files.uploader_id = sqlc.arg(uploader_id)
          AND files.access_hash = (sqlc.arg(access_hashes)::bigint[])[requested.request_order]
          AND files.stored = true
          AND files.media_kind = 'document'
WHERE NOT EXISTS (
    SELECT 1
    FROM user_created_sticker_set_items
    WHERE user_created_sticker_set_items.file_id = files.id
)
ORDER BY requested.request_order;

-- name: UserCreatedStickerSetByShortName :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM live_user_created_sticker_sets
WHERE lower(short_name) = lower($1) AND is_live = true;

-- name: UserCreatedStickerSetByID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM live_user_created_sticker_sets
WHERE id = $1 AND access_hash = $2 AND is_live = true;

-- name: UserCreatedStickerSetByInstalledID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM live_user_created_sticker_sets
WHERE id = $1 AND is_live = true;

-- The installed-set list getters need only summary metadata; set item rows are
-- loaded only by full getStickerSet and media hydration paths.
-- name: UserCreatedStickerSetsByInstalledIDs :many
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM live_user_created_sticker_sets
WHERE id = ANY($1::bigint[]) AND is_live = true
ORDER BY id;

-- Set items reference files independently of message state. The set's live
-- state controls serving, but does not decide whether a file remains referenced.
-- name: UserCreatedStickerSetItems :many
SELECT i.set_id, i.position, i.file_id, f.access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM user_created_sticker_set_items i
JOIN live_user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.set_id = $1 AND f.stored = true
ORDER BY i.position;

-- name: UserCreatedStickerSetItemsBySetIDs :many
SELECT i.set_id, i.position, i.file_id, f.access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM user_created_sticker_set_items i
JOIN live_user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.set_id = ANY($1::bigint[]) AND f.stored = true
ORDER BY i.set_id, i.position;

-- name: UserCreatedStickerSetItemsByFileIDs :many
SELECT s.id AS set_id, s.access_hash AS set_access_hash, s.creator_id,
       s.title, s.short_name, s.software, s.kind, s.text_color, s.is_live,
       s.created_at, i.position, i.file_id, f.access_hash AS file_access_hash,
       f.size, f.date, i.emoji, i.keywords, i.format, i.width, i.height,
       i.duration_ns, COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM user_created_sticker_set_items i
JOIN live_user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.file_id = ANY($1::bigint[]) AND f.stored = true
ORDER BY i.file_id;

-- name: UserStickerSetShortNameExists :one
SELECT EXISTS (
    SELECT 1 FROM user_created_sticker_sets WHERE lower(short_name) = lower($1)
);

-- name: UserStickerSetOccupiedShortNames :many
SELECT lower(short_name) AS short_name
FROM user_created_sticker_sets
WHERE lower(short_name) = ANY($1::text[]);

-- name: UserStickerDocumentExists :one
SELECT EXISTS (
    SELECT 1
    FROM user_created_sticker_set_items i
    JOIN live_user_created_sticker_sets s ON s.id = i.set_id
    JOIN files f ON f.id = i.file_id
    WHERE f.id = $1 AND f.access_hash = $2 AND f.stored = true
);

-- name: UserStickerDocumentSetIDs :many
SELECT i.file_id, i.set_id
FROM user_created_sticker_set_items i
JOIN live_user_created_sticker_sets s ON s.id = i.set_id
WHERE i.file_id = ANY($1::bigint[])
ORDER BY i.set_id, i.file_id;

-- name: LockLiveUserStickerSet :one
SELECT id FROM live_user_created_sticker_sets
WHERE id = $1
FOR SHARE;

-- Lock order for sticker-set writes is owner users FOR UPDATE, this interlock,
-- set-row locks, then file-reference locks in ascending file ID order. A path
-- may omit a class but must not acquire an earlier class after a later one.
-- Fingerprinting is done before opening the transaction; this lock serializes
-- the final blocked-content check with takedown hash publication.
-- name: LockStickerTakedownInterlock :exec
SELECT pg_advisory_xact_lock(1145132116, 237731251);

-- name: UserStickerBlockedContentExists :one
SELECT EXISTS (
    SELECT 1
    FROM user_created_sticker_set_items i
    JOIN user_created_sticker_sets s ON s.id = i.set_id
    WHERE s.is_live = false
      AND i.content_sha256 = ANY(sqlc.arg(content_sha256)::bytea[])
);

-- name: TakedownUserStickerSetFiles :many
SELECT f.id AS file_id, f.access_hash, f.size, f.stored
FROM user_created_sticker_set_items i
JOIN files f ON f.id = i.file_id
WHERE i.set_id = sqlc.arg(set_id)
ORDER BY f.id;

-- name: UpdateUserStickerSetItemContentSHA256 :execrows
UPDATE user_created_sticker_set_items
SET content_sha256 = sqlc.arg(content_sha256)
WHERE set_id = sqlc.arg(set_id) AND file_id = sqlc.arg(file_id);

-- name: AdminLockUserStickerSetByID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM user_created_sticker_sets
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: AdminReadUserStickerSetByID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM user_created_sticker_sets
WHERE id = sqlc.arg(id);

-- name: AdminLockUserStickerSetByShortName :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM user_created_sticker_sets
WHERE lower(short_name) = lower(sqlc.arg(short_name))
FOR UPDATE;

-- name: AdminReadUserStickerSetByShortName :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM user_created_sticker_sets
WHERE lower(short_name) = lower(sqlc.arg(short_name));

-- name: AdminSetUserStickerSetLive :execrows
UPDATE user_created_sticker_sets
SET is_live = sqlc.arg(is_live)
WHERE id = sqlc.arg(id);

-- name: InsertAdminStickerSetEvent :exec
INSERT INTO admin_sticker_set_events
    (action, set_id, set_title, set_short_name, reason, session_fingerprint)
VALUES (
    sqlc.arg(action), sqlc.arg(set_id), sqlc.arg(set_title),
    sqlc.arg(set_short_name), sqlc.arg(reason), sqlc.arg(session_fingerprint)
);

-- name: AdminUserStickerSetsPage :many
SELECT id, title, short_name, creator_id, created_at, software, is_live
FROM user_created_sticker_sets
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit_count) OFFSET sqlc.arg(offset_count);

-- name: AdminStickerSetEventsPage :many
SELECT id, action, set_id, set_title, set_short_name, reason,
       session_fingerprint, created_at
FROM admin_sticker_set_events
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit_count) OFFSET sqlc.arg(offset_count);

-- name: InstalledStickerSetCount :one
SELECT count(*)::bigint
FROM user_sticker_sets
WHERE user_id = $1;

-- name: UserStickerSetAlreadyInstalled :one
SELECT EXISTS (
    SELECT 1 FROM user_sticker_sets WHERE user_id = $1 AND set_id = $2
);
