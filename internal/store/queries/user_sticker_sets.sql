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
          text_color, item_count, set_hash, is_live, created_at, thumb_file_id,
          thumb_format, thumb_size, thumb_width, thumb_height, thumb_duration_ns,
          thumb_version, thumb_document_id;

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
AND NOT EXISTS (
    SELECT 1
    FROM user_created_sticker_sets
    WHERE user_created_sticker_sets.thumb_file_id = files.id
)
ORDER BY requested.request_order;

-- Add may reactivate a previously removed item from the same set, but a live
-- item, another set's historical item, or any set thumbnail still owns the file.
-- name: StickerFilesForUserSet :many
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
    FROM user_created_sticker_set_items i
    WHERE i.file_id = files.id
      AND (i.set_id <> sqlc.arg(set_id) OR i.is_active = true)
)
AND NOT EXISTS (
    SELECT 1
    FROM user_created_sticker_sets s
    WHERE s.thumb_file_id = files.id
)
ORDER BY requested.request_order;

-- name: UserCreatedStickerSetByShortName :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at, thumb_file_id,
       thumb_format, thumb_size, thumb_width, thumb_height, thumb_duration_ns,
       thumb_version, thumb_document_id
FROM user_created_sticker_sets
WHERE lower(short_name) = lower($1) AND is_live = true;

-- name: UserCreatedStickerSetByID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at, thumb_file_id,
       thumb_format, thumb_size, thumb_width, thumb_height, thumb_duration_ns,
       thumb_version, thumb_document_id
FROM user_created_sticker_sets
WHERE id = $1 AND access_hash = $2 AND is_live = true;

-- This candidate search handles literal patterns with a word run that
-- produces pg_trgm terms. The encoded query below handles other patterns.
-- The caller must bulk-resolve each candidate with its exact hash.
-- name: UserCreatedStickerSetSearchCandidates :many
SELECT id, access_hash
FROM user_created_sticker_sets
WHERE is_live = true
  AND (lower(title) LIKE '%' || lower($1) || '%' ESCAPE E'\\'
       OR lower(short_name) LIKE '%' || lower($1) || '%' ESCAPE E'\\')
ORDER BY id
LIMIT 50;

-- Encode code points as fixed-width alphanumeric hex so pg_trgm can index
-- substrings containing separators, which do not produce word trigrams in
-- the source. strpos keeps the indexed candidate match exact.
-- name: UserCreatedStickerSetSearchCandidatesByEncodedSubstring :many
SELECT id, access_hash
FROM user_created_sticker_sets
WHERE is_live = true
  AND (
      public.user_sticker_set_search_key(lower(title))
          LIKE '%' || public.user_sticker_set_search_key(lower($1)) || '%'
      OR public.user_sticker_set_search_key(lower(short_name))
          LIKE '%' || public.user_sticker_set_search_key(lower($1)) || '%'
  )
  AND (strpos(lower(title), lower($1)) > 0 OR strpos(lower(short_name), lower($1)) > 0)
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
       s.thumb_file_id, s.thumb_format, s.thumb_size, s.thumb_width,
       s.thumb_height, s.thumb_duration_ns, s.thumb_version, s.thumb_document_id,
       i.position, i.file_id, f.access_hash AS item_access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM candidates c
JOIN user_created_sticker_sets s
  ON s.id = c.id AND s.access_hash = c.access_hash AND s.is_live = true
JOIN user_created_sticker_set_items i ON i.set_id = s.id AND i.is_active = true
JOIN files f ON f.id = i.file_id AND f.stored = true
ORDER BY s.id, i.position;

-- name: UserCreatedStickerSetByInstalledID :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at, thumb_file_id,
       thumb_format, thumb_size, thumb_width, thumb_height, thumb_duration_ns,
       thumb_version, thumb_document_id
FROM user_created_sticker_sets
WHERE id = $1 AND is_live = true;

-- The installed-set list getters need only summary metadata; set item rows are
-- loaded only by full getStickerSet and media hydration paths.
-- name: UserCreatedStickerSetsByInstalledIDs :many
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at, thumb_file_id,
       thumb_format, thumb_size, thumb_width, thumb_height, thumb_duration_ns,
       thumb_version, thumb_document_id
FROM user_created_sticker_sets
WHERE id = ANY($1::bigint[]) AND is_live = true
ORDER BY id;

-- Every mutation takes the set row FOR UPDATE before editing items or metadata.
-- Messages take it FOR SHARE before accepting a sticker send or forward.
-- name: LockOwnedLiveUserStickerSet :one
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at, thumb_file_id,
       thumb_format, thumb_size, thumb_width, thumb_height, thumb_duration_ns,
       thumb_version, thumb_document_id
FROM user_created_sticker_sets
WHERE id = sqlc.arg(id) AND creator_id = sqlc.arg(creator_id) AND is_live = true
FOR UPDATE;

-- A sticker-only edit locates the set through an exact stored document identity.
-- name: LockOwnedLiveUserStickerSetBySticker :one
SELECT s.id, s.access_hash, s.creator_id, s.title, s.short_name, s.software, s.kind,
       s.text_color, s.item_count, s.set_hash, s.is_live, s.created_at,
       s.thumb_file_id, s.thumb_format, s.thumb_size, s.thumb_width,
       s.thumb_height, s.thumb_duration_ns, s.thumb_version, s.thumb_document_id
FROM user_created_sticker_sets s
JOIN user_created_sticker_set_items i ON i.set_id = s.id AND i.is_active = true
JOIN files f ON f.id = i.file_id
WHERE s.creator_id = sqlc.arg(creator_id) AND s.is_live = true
  AND f.id = sqlc.arg(file_id) AND f.access_hash = sqlc.arg(access_hash)
FOR UPDATE OF s;

-- API validation needs the set kind before checking optional sticker fields.
-- The write path repeats this lookup under a row lock before making changes.
-- name: UserCreatedStickerSetByOwnedSticker :one
SELECT s.id, s.access_hash, s.creator_id, s.title, s.short_name, s.software, s.kind,
       s.text_color, s.item_count, s.set_hash, s.is_live, s.created_at,
       s.thumb_file_id, s.thumb_format, s.thumb_size, s.thumb_width,
       s.thumb_height, s.thumb_duration_ns, s.thumb_version, s.thumb_document_id
FROM user_created_sticker_sets s
JOIN user_created_sticker_set_items i ON i.set_id = s.id AND i.is_active = true
JOIN files f ON f.id = i.file_id
WHERE s.creator_id = sqlc.arg(creator_id) AND s.is_live = true
  AND f.id = sqlc.arg(file_id) AND f.access_hash = sqlc.arg(access_hash);

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
  AND i.is_active = true
ORDER BY i.position;

-- name: UserCreatedStickerSetItemsBySetIDs :many
SELECT i.set_id, i.position, i.file_id, f.access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords
FROM user_created_sticker_set_items i
JOIN user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.set_id = ANY($1::bigint[]) AND s.is_live = true AND f.stored = true
  AND i.is_active = true
ORDER BY i.set_id, i.position;

-- Mutation rows are limited to the active set, which is capped at 200 items.
-- name: UserCreatedStickerSetItemsForMutation :many
SELECT i.set_id, i.position, i.file_id, f.access_hash, f.size, f.date,
       i.emoji, i.keywords, i.format, i.width, i.height, i.duration_ns,
       COALESCE(i.mask_coords::text, '')::varchar AS mask_coords, i.is_active
FROM user_created_sticker_set_items i
JOIN files f ON f.id = i.file_id
WHERE i.set_id = $1 AND f.stored = true AND i.is_active = true
ORDER BY i.position;

-- Archived rows left by the previous dense-position scheme can occupy the
-- small active/temporary range. This bounded lookup lets the first edit move
-- only those rows out of the reserved range without reading lifetime history.
-- name: UserCreatedStickerSetArchivedItemsInPositionRange :many
SELECT file_id
FROM user_created_sticker_set_items
WHERE set_id = sqlc.arg(set_id) AND is_active = false
  AND position <= sqlc.arg(max_position)
ORDER BY position;

-- name: UserCreatedStickerSetMaxPosition :one
SELECT COALESCE(MAX(position)::bigint, 0)::bigint
FROM user_created_sticker_set_items
WHERE set_id = $1;

-- name: UserCreatedStickerSetItemExistsInSet :one
SELECT EXISTS (
    SELECT 1 FROM user_created_sticker_set_items
    WHERE set_id = sqlc.arg(set_id) AND file_id = sqlc.arg(file_id)
);

-- name: UserCreatedStickerSetItemsByFileIDs :many
SELECT s.id AS set_id, s.access_hash AS set_access_hash, s.creator_id,
       s.title, s.short_name, s.software, s.kind, s.text_color, s.is_live,
       s.created_at, i.position, i.file_id, f.access_hash AS file_access_hash,
       f.size, f.date, i.emoji, i.keywords, i.format, i.width, i.height,
       i.duration_ns, COALESCE(i.mask_coords::text, '')::varchar AS mask_coords,
       i.is_active
FROM user_created_sticker_set_items i
JOIN user_created_sticker_sets s ON s.id = i.set_id
JOIN files f ON f.id = i.file_id
WHERE i.file_id = ANY($1::bigint[]) AND f.stored = true
ORDER BY i.file_id;

-- Only a caller-visible stored message or poll copy keeps a removed or deleted
-- custom emoji document resolvable. Current set members are served from set state.
-- name: UserStickerSetMessageReferencedFileIDs :many
WITH requested AS (
    SELECT DISTINCT unnest(sqlc.arg(file_ids)::text[]) AS file_id
), -- Filter channel memberships before resolving channel history.
visible_channels AS MATERIALIZED (
    SELECT cp.channel_id, cp.available_min_id
    FROM channel_participants cp
    JOIN channels c ON c.id = cp.channel_id
    WHERE cp.user_id = sqlc.arg(owner_id)
      AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      AND c.deleted_at IS NULL
), channel_candidates AS MATERIALIZED (
    SELECT cm.entities
    FROM visible_channels vc
    JOIN channel_messages cm
      ON cm.channel_id = vc.channel_id
     AND cm.local_id > vc.available_min_id
    WHERE cm.deleted = false
      AND cm.entities IS NOT NULL
      AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
      AND cardinality(message_custom_emoji_document_ids(cm.entities)) > 0
      AND message_custom_emoji_document_ids(cm.entities) && sqlc.arg(file_ids)::text[]
), visible_entities AS (
    SELECT m.entities
    FROM messages m
    WHERE m.owner_id = sqlc.arg(owner_id)
      AND m.deleted = false
      AND m.entities IS NOT NULL
      AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
      AND cardinality(message_custom_emoji_document_ids(m.entities)) > 0
      AND message_custom_emoji_document_ids(m.entities) && sqlc.arg(file_ids)::text[]
    UNION ALL
    SELECT cm.entities
    FROM channel_candidates cm
)
SELECT DISTINCT requested.file_id::bigint
FROM visible_entities
CROSS JOIN LATERAL jsonb_array_elements(
    CASE
        WHEN jsonb_typeof(visible_entities.entities->'entities') = 'array'
            THEN visible_entities.entities->'entities'
        ELSE '[]'::jsonb
    END
) AS entity(value)
JOIN requested ON entity.value->>'argument' = requested.file_id
WHERE jsonb_typeof(entity.value->'type') = 'number'
  AND entity.value->>'type' = sqlc.arg(entity_type)::int::text
  AND jsonb_typeof(entity.value->'argument') = 'number'
UNION
SELECT requested.file_id::bigint
FROM poll_message_copies copy
JOIN messages m ON m.owner_id = copy.owner_id AND m.local_id = copy.local_id
CROSS JOIN LATERAL unnest(copy.custom_emoji_document_ids) AS document(file_id)
JOIN requested ON requested.file_id = document.file_id
WHERE copy.owner_id = sqlc.arg(owner_id)
  AND cardinality(copy.custom_emoji_document_ids) > 0
  AND copy.custom_emoji_document_ids && sqlc.arg(file_ids)::text[]
  AND m.deleted = false
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
UNION
SELECT requested.file_id::bigint
FROM visible_channels vc
JOIN channel_poll_messages copy ON copy.channel_id = vc.channel_id
JOIN channel_messages cm
  ON cm.channel_id = copy.channel_id AND cm.local_id = copy.local_id
CROSS JOIN LATERAL unnest(copy.custom_emoji_document_ids) AS document(file_id)
JOIN requested ON requested.file_id = document.file_id
WHERE copy.custom_emoji_document_ids && sqlc.arg(file_ids)::text[]
  AND cardinality(copy.custom_emoji_document_ids) > 0
  AND cm.local_id > vc.available_min_id
  AND cm.deleted = false
  AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp());

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
      AND i.is_active = true
);

-- name: UserStickerDocumentSetIDs :many
SELECT i.file_id, i.set_id
FROM user_created_sticker_set_items i
JOIN user_created_sticker_sets s ON s.id = i.set_id
WHERE i.file_id = ANY($1::bigint[]) AND s.is_live = true AND i.is_active = true
ORDER BY i.set_id, i.file_id;

-- name: LockLiveUserStickerSet :one
SELECT id FROM user_created_sticker_sets
WHERE id = $1 AND is_live = true
FOR SHARE;

-- name: ArchiveUserCreatedStickerSetItem :execrows
UPDATE user_created_sticker_set_items
SET is_active = false, position = sqlc.arg(position)
WHERE set_id = sqlc.arg(set_id) AND file_id = sqlc.arg(file_id) AND is_active = true;

-- name: ReactivateUserCreatedStickerSetItem :execrows
UPDATE user_created_sticker_set_items
SET is_active = true, position = sqlc.arg(position), emoji = sqlc.arg(emoji),
    keywords = sqlc.arg(keywords), format = sqlc.arg(format), width = sqlc.arg(width),
    height = sqlc.arg(height), duration_ns = sqlc.arg(duration_ns), mask_coords = sqlc.arg(mask_coords),
    content_sha256 = sqlc.arg(content_sha256)
WHERE set_id = sqlc.arg(set_id) AND file_id = sqlc.arg(file_id) AND is_active = false;

-- name: UpdateUserCreatedStickerSetItemDetails :execrows
UPDATE user_created_sticker_set_items
SET emoji = $3, keywords = $4, mask_coords = $5
WHERE set_id = $1 AND file_id = $2 AND is_active = true;

-- Active rows are at most 200 and use positions 0..199. Inactive legacy rows
-- in the active and temporary ranges are moved to the archive tail first.
-- name: ShiftUserCreatedStickerSetItemPositions :execrows
UPDATE user_created_sticker_set_items
SET position = position + sqlc.arg(shift_by)::int
WHERE set_id = sqlc.arg(set_id) AND is_active = true;

-- name: SetUserCreatedStickerSetItemPosition :execrows
UPDATE user_created_sticker_set_items
SET position = $3
WHERE set_id = $1 AND file_id = $2;

-- name: UpdateUserCreatedStickerSetCountAndHash :execrows
UPDATE user_created_sticker_sets
SET item_count = $2, set_hash = $3
WHERE id = $1;

-- A removed item cannot remain the custom emoji set's selected thumbnail.
-- The set row is already locked by the enclosing mutation transaction.
-- name: ClearRemovedUserStickerSetThumbDocument :execrows
UPDATE user_created_sticker_sets
SET thumb_document_id = 0,
    thumb_version = sqlc.arg(thumb_version)
WHERE id = sqlc.arg(id)
  AND thumb_document_id = sqlc.arg(thumb_document_id)
  AND kind = 'emojis'
  AND is_live = true;

-- name: RenameUserCreatedStickerSet :execrows
UPDATE user_created_sticker_sets
SET title = $2, set_hash = $3
WHERE id = $1;

-- name: UpdateUserCreatedStickerSetThumb :execrows
UPDATE user_created_sticker_sets
SET thumb_file_id = sqlc.narg(thumb_file_id),
    thumb_format = sqlc.narg(thumb_format),
    thumb_size = sqlc.narg(thumb_size),
    thumb_width = sqlc.narg(thumb_width),
    thumb_height = sqlc.narg(thumb_height),
    thumb_duration_ns = sqlc.arg(thumb_duration_ns),
    thumb_version = sqlc.arg(thumb_version),
    thumb_document_id = sqlc.arg(thumb_document_id),
    set_hash = sqlc.arg(set_hash)
WHERE id = sqlc.arg(id);

-- name: DeleteUserCreatedStickerSet :execrows
UPDATE user_created_sticker_sets
SET is_live = false,
    thumb_file_id = NULL,
    thumb_format = NULL,
    thumb_size = NULL,
    thumb_width = NULL,
    thumb_height = NULL,
    thumb_duration_ns = 0,
    thumb_document_id = 0
WHERE id = $1 AND is_live = true;

-- Deleting a user set also drops each install row; historical item rows remain
-- as message-media references and keep their uploaded files from erasure.
-- name: DeleteInstalledUserStickerSet :execrows
DELETE FROM user_sticker_sets
WHERE set_id = $1;

-- name: UserCreatedStickerSetThumbnailForDownload :one
SELECT f.*
FROM user_created_sticker_sets s
JOIN files f ON f.id = s.thumb_file_id
WHERE s.id = sqlc.arg(id) AND s.access_hash = sqlc.arg(access_hash)
  AND s.thumb_version = sqlc.arg(thumb_version) AND s.is_live = true
  AND f.stored = true;

-- name: UserCreatedStickerSetItemExists :one
SELECT EXISTS (
    SELECT 1 FROM user_created_sticker_set_items
    WHERE set_id = sqlc.arg(set_id) AND file_id = sqlc.arg(file_id) AND is_active = true
);

-- Lock order for sticker-set writes is owner users FOR UPDATE, this interlock,
-- set-row locks, then file-reference locks in ascending file ID order. A path
-- may omit a class but must not acquire an earlier class after a later one.
-- Fingerprinting is done before opening the transaction; the shared/exclusive
-- modes serialize the final blocked-content check with takedown hash publication.
-- Create and add transactions share this gate. Takedown and restore use the
-- exclusive lock below, so they wait before publishing blocked-content state.
-- name: LockStickerSetCreateInterlock :exec
SELECT pg_advisory_xact_lock_shared(1145132116, 237731251);

-- name: LockStickerTakedownInterlock :exec
SELECT pg_advisory_xact_lock(1145132116, 237731251);

-- name: UserStickerBlockedContentExists :one
SELECT EXISTS (
    SELECT 1
    FROM user_created_sticker_set_items i
    JOIN user_created_sticker_sets s ON s.id = i.set_id
    WHERE s.is_live = false AND i.is_active = true
      AND (
          SELECT e.action
          FROM admin_sticker_set_events e
          WHERE e.set_id = s.id
          ORDER BY e.id DESC
          LIMIT 1
      ) = 'takedown'
      AND i.content_sha256 = ANY(sqlc.arg(content_sha256)::bytea[])
);

-- name: TakedownUserStickerSetFiles :many
SELECT f.id AS file_id, f.access_hash, f.size, f.stored
FROM user_created_sticker_set_items i
JOIN files f ON f.id = i.file_id
WHERE i.set_id = sqlc.arg(set_id) AND i.is_active = true
ORDER BY f.id;

-- name: UpdateUserStickerSetItemContentSHA256 :execrows
UPDATE user_created_sticker_set_items
SET content_sha256 = sqlc.arg(content_sha256)
WHERE set_id = sqlc.arg(set_id) AND file_id = sqlc.arg(file_id);

-- name: AdminLockUserStickerSetByID :one
SELECT *
FROM user_created_sticker_sets
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: AdminReadUserStickerSetByID :one
SELECT *
FROM user_created_sticker_sets
WHERE id = sqlc.arg(id);

-- name: AdminLockUserStickerSetByShortName :one
SELECT *
FROM user_created_sticker_sets
WHERE lower(short_name) = lower(sqlc.arg(short_name))
FOR UPDATE;

-- name: AdminReadUserStickerSetByShortName :one
SELECT *
FROM user_created_sticker_sets
WHERE lower(short_name) = lower(sqlc.arg(short_name));

-- Owner deletion writes no admin event. A latest takedown event distinguishes
-- an administratively removed, non-live set from one deleted by its owner.
-- name: AdminLatestUserStickerSetEventAction :one
SELECT action
FROM admin_sticker_set_events
WHERE set_id = sqlc.arg(set_id)
ORDER BY id DESC
LIMIT 1;

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
