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
    (set_id, position, file_id, emoji, keywords, format, width, height, duration_ns, mask_coords)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
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
FROM user_created_sticker_sets
WHERE lower(short_name) = lower($1) AND is_live = true;

-- name: UserCreatedStickerSetByID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM user_created_sticker_sets
WHERE id = $1 AND access_hash = $2 AND is_live = true;

-- Search bounds candidates and uses trigram indexes for literal substring
-- matches. The caller must bulk-resolve each candidate with its exact hash.
-- The store rejects queries shorter than three Unicode characters, because
-- their substring patterns cannot provide a trigram for index pruning.
-- name: UserCreatedStickerSetSearchCandidates :many
SELECT id, access_hash
FROM user_created_sticker_sets
WHERE is_live = true
  AND (lower(title) LIKE '%' || lower($1) || '%' ESCAPE E'\\'
       OR lower(short_name) LIKE '%' || lower($1) || '%' ESCAPE E'\\')
ORDER BY id
LIMIT 50;

-- Bulk resolution preserves the exact candidate access hash and live-set
-- checks, then loads all stored item metadata in one bounded query.
-- name: UserCreatedStickerSetsBySearchCandidates :many
WITH candidates AS (
    SELECT ids.set_id AS id, candidate_accesses.access_hash
    FROM unnest(sqlc.arg(set_ids)::bigint[]) WITH ORDINALITY AS ids(set_id, ord)
    JOIN unnest(sqlc.arg(access_hashes)::bigint[]) WITH ORDINALITY AS candidate_accesses(access_hash, ord) USING (ord)
)
SELECT s.id, s.access_hash, s.creator_id, s.title, s.short_name, s.software,
       s.kind, s.text_color, s.item_count, s.set_hash, s.is_live, s.created_at,
       i.position, i.file_id, f.access_hash AS item_access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM candidates c
JOIN user_created_sticker_sets s
  ON s.id = c.id AND s.access_hash = c.access_hash AND s.is_live = true
JOIN user_created_sticker_set_items i ON i.set_id = s.id
JOIN files f ON f.id = i.file_id AND f.stored = true
ORDER BY s.id, i.position;

-- name: UserCreatedStickerSetByInstalledID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM user_created_sticker_sets
WHERE id = $1 AND is_live = true;

-- The installed-set list getters need only summary metadata; set item rows are
-- loaded only by full getStickerSet and media hydration paths.
-- name: UserCreatedStickerSetsByInstalledIDs :many
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
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
