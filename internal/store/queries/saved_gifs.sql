-- InsertSavedGIF uses the same live-message and channel-membership conditions
-- as FileForDownload. A saved row records a bookmark, never a file entitlement.
-- Advancing beyond the owner's current maximum makes a re-save move to the
-- front even when calls happen within the same timestamp tick.
-- name: InsertSavedGIF :execrows
INSERT INTO saved_gifs (owner_id, file_id, saved_at)
SELECT $1, f.id,
       GREATEST(
           clock_timestamp(),
           coalesce((SELECT max(saved_at) FROM saved_gifs WHERE owner_id = $1), '-infinity'::timestamptz)
               + interval '1 microsecond'
       )
FROM files f
WHERE f.id = $2 AND f.access_hash = $3 AND f.stored = true
  AND f.media_kind = 'document'
  AND f.subtype_rights @> ARRAY['send_gifs']::TEXT[]
  AND f.mime_type IN ('video/mp4', 'image/gif')
  AND (
      EXISTS (
          SELECT 1 FROM messages m
          WHERE m.owner_id = $1 AND m.file_id = f.id AND m.deleted = false
            AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
      )
      OR EXISTS (
          SELECT 1 FROM channel_messages cm
          JOIN channel_participants cp
            ON cp.channel_id = cm.channel_id AND cp.user_id = $1
          WHERE cm.file_id = f.id
            AND cm.deleted = false
            AND cm.local_id > cp.available_min_id
            AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
            AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      )
  )
ON CONFLICT (owner_id, file_id) DO UPDATE SET saved_at = EXCLUDED.saved_at;

-- TrimSavedGIFs keeps only the newest bounded rows, including bookmarks whose
-- source entitlement was later revoked. Those stale rows are inert and age out
-- under the same account bound.
-- name: TrimSavedGIFs :execrows
DELETE FROM saved_gifs AS stale
WHERE stale.owner_id = $1 AND stale.file_id NOT IN (
    SELECT retained.file_id FROM saved_gifs AS retained
    WHERE retained.owner_id = $1
    ORDER BY retained.saved_at DESC, retained.file_id DESC
    LIMIT $2
);

-- SavedGIFs repeats the FileForDownload entitlement predicate at read time.
-- Expired, deleted, banned, departed, and history-floored source messages stop
-- appearing without granting any extra upload.getFile access.
-- name: SavedGIFs :many
SELECT f.* FROM saved_gifs sg
JOIN files f ON f.id = sg.file_id
WHERE sg.owner_id = $1
  AND f.stored = true
  AND f.media_kind = 'document'
  AND f.subtype_rights @> ARRAY['send_gifs']::TEXT[]
  AND f.mime_type IN ('video/mp4', 'image/gif')
  AND (
      EXISTS (
          SELECT 1 FROM messages m
          WHERE m.owner_id = $1 AND m.file_id = f.id AND m.deleted = false
            AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
      )
      OR EXISTS (
          SELECT 1 FROM channel_messages cm
          JOIN channel_participants cp
            ON cp.channel_id = cm.channel_id AND cp.user_id = $1
          WHERE cm.file_id = f.id
            AND cm.deleted = false
            AND cm.local_id > cp.available_min_id
            AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
            AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      )
  )
ORDER BY sg.saved_at DESC, sg.file_id DESC
LIMIT $2;

-- UnsaveSavedGIF only removes the caller's own bookmark; authorization and the
-- access hash are irrelevant because no file metadata or bytes are returned.
-- name: UnsaveSavedGIF :execrows
DELETE FROM saved_gifs WHERE owner_id = $1 AND file_id = $2;
