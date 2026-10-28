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
    (access_hash, creator_id, title, short_name, software, kind, text_color)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, access_hash, creator_id, title, short_name, software, kind,
          text_color, is_live, created_at;

-- name: InsertUserCreatedStickerSetItem :execrows
INSERT INTO user_created_sticker_set_items
    (set_id, position, file_id, emoji, keywords, format, width, height, duration_ns, mask_coords)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (file_id) DO NOTHING;

-- Sticker inputs are resolved only through a caller-owned, stored document row
-- and its exact access hash. Client MIME and attributes are not selected.
-- name: StickerFileForOwner :one
SELECT id, uploader_id, access_hash, size, mime_type, file_name, stored, date,
       subtype_rights, media_kind, width, height
FROM files
WHERE id = $1 AND uploader_id = $2 AND access_hash = $3
  AND stored = true AND media_kind = 'document';

-- name: UserCreatedStickerSetByShortName :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, is_live, created_at
FROM user_created_sticker_sets
WHERE lower(short_name) = lower($1) AND is_live = true;

-- name: UserCreatedStickerSetByID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, is_live, created_at
FROM user_created_sticker_sets
WHERE id = $1 AND access_hash = $2 AND is_live = true;

-- Search returns only credentials for live sets. Callers must resolve these
-- through UserCreatedStickerSetByID before exposing a result.
-- name: UserCreatedStickerSetSearchCandidates :many
SELECT id, access_hash
FROM user_created_sticker_sets
WHERE is_live = true
  AND (strpos(lower(title), lower($1)) > 0 OR strpos(lower(short_name), lower($1)) > 0)
ORDER BY id;

-- name: UserCreatedStickerSetByInstalledID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, is_live, created_at
FROM user_created_sticker_sets
WHERE id = $1 AND is_live = true;

-- The installed-set getters resolve every positive id together, then fetch
-- all their ordered items in one query to keep the 200-set cap bounded.
-- name: UserCreatedStickerSetsByInstalledIDs :many
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, is_live, created_at
FROM user_created_sticker_sets
WHERE id = ANY($1::bigint[]) AND is_live = true
ORDER BY id;

-- Set items reference files independently of message state. The set's live
-- state controls serving, but does not decide whether a file remains referenced.
-- name: UserCreatedStickerSetItems :many
SELECT i.set_id, i.position, i.file_id, f.access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM user_created_sticker_set_items i
JOIN user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.set_id = $1 AND s.is_live = true AND f.stored = true
ORDER BY i.position;

-- name: UserCreatedStickerSetItemsBySetIDs :many
SELECT i.set_id, i.position, i.file_id, f.access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM user_created_sticker_set_items i
JOIN user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.set_id = ANY($1::bigint[]) AND s.is_live = true AND f.stored = true
ORDER BY i.set_id, i.position;

-- name: UserCreatedStickerSetItemsByFileIDs :many
SELECT s.id AS set_id, s.access_hash AS set_access_hash, s.creator_id,
       s.title, s.short_name, s.software, s.kind, s.text_color, s.is_live,
       s.created_at, i.position, i.file_id, f.access_hash AS file_access_hash,
       f.size, f.date, i.emoji, i.keywords, i.format, i.width, i.height,
       i.duration_ns, COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM user_created_sticker_set_items i
JOIN user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.file_id = ANY($1::bigint[]) AND s.is_live = true AND f.stored = true
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
    JOIN user_created_sticker_sets s ON s.id = i.set_id
    JOIN files f ON f.id = i.file_id
    WHERE f.id = $1 AND f.access_hash = $2 AND f.stored = true AND s.is_live = true
);

-- name: UserStickerDocumentSetIDs :many
SELECT i.file_id, i.set_id
FROM user_created_sticker_set_items i
JOIN user_created_sticker_sets s ON s.id = i.set_id
WHERE i.file_id = ANY($1::bigint[]) AND s.is_live = true
ORDER BY i.set_id, i.file_id;

-- name: LockLiveUserStickerSet :one
SELECT id FROM user_created_sticker_sets
WHERE id = $1 AND is_live = true
FOR SHARE;

-- name: InstalledStickerSetCount :one
SELECT count(*)::bigint
FROM user_sticker_sets
WHERE user_id = $1;

-- name: UserStickerSetAlreadyInstalled :one
SELECT EXISTS (
    SELECT 1 FROM user_sticker_sets WHERE user_id = $1 AND set_id = $2
);
