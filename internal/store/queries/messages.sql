-- name: InsertMessage :exec
INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out, random_id, peer_local_id,
                      fanout_id, action_type, action_user_id, file_id, reply_to_msg_id,
                      fwd_from_id, fwd_date, fwd_channel_id, fwd_channel_post, reply_to_trusted)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19);

-- ActiveOrdinaryMessageInDialog is the authoritative lookup for a client reply.
-- It deliberately checks owner-local id, exact peer namespace, live state, and
-- ordinary-message type in the statement that runs inside the send transaction.
-- name: ActiveOrdinaryMessageInDialog :one
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND local_id = sqlc.arg(local_id)::bigint
  AND peer_type = sqlc.arg(peer_type)::smallint
  AND peer_id = sqlc.arg(peer_id)::bigint
  AND deleted = false
  AND action_type = 0;

-- name: MessageByOwnerLocal :one
SELECT * FROM messages WHERE owner_id = $1 AND local_id = $2;

-- name: MessageByRandomID :one
SELECT * FROM messages WHERE owner_id = $1 AND random_id = $2 AND random_id <> 0;

-- NextFanoutID allocates the id shared by every per-member copy of one chat
-- message. One value per fan-out, never 0.
-- name: NextFanoutID :one
SELECT nextval('message_fanout_seq')::bigint AS fanout_id;

-- MessagesByFanout returns every per-member copy of one chat message, ascending
-- by owner_id so a caller can take its advisory locks in that order. The
-- `fanout_id <> 0` predicate is not redundant with the equality: 0 is the "not a
-- chat message" sentinel every 1:1 row carries, so a zero argument would select
-- the entire table instead of nothing.
-- name: MessagesByFanout :many
SELECT * FROM messages
WHERE fanout_id = $1 AND fanout_id <> 0
ORDER BY owner_id;

-- name: HistoryPage :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)
  AND peer_type = sqlc.arg(peer_type)
  AND peer_id = sqlc.arg(peer_id)
  AND deleted = false
  AND (sqlc.arg(offset_id)::bigint = 0 OR local_id < sqlc.arg(offset_id)::bigint)
ORDER BY local_id DESC
LIMIT sqlc.arg(lim)::int
OFFSET GREATEST(0::bigint, sqlc.arg(add_offset)::bigint);

-- HistoryPageAround handles negative add_offset by converting offset_id to its
-- ordinal in the owner's filtered newest-first history before selecting a page.
-- name: HistoryPageAround :many
WITH page_offset AS (
    SELECT GREATEST(0::bigint, COUNT(*) + sqlc.arg(add_offset)::bigint) AS skip
    FROM messages AS offset_message
    WHERE offset_message.owner_id = sqlc.arg(owner_id)
      AND offset_message.peer_type = sqlc.arg(peer_type)
      AND offset_message.peer_id = sqlc.arg(peer_id)
      AND offset_message.deleted = false
      AND offset_message.local_id >= sqlc.arg(offset_id)::bigint
)
SELECT page_message.* FROM messages AS page_message
WHERE page_message.owner_id = sqlc.arg(owner_id)
  AND page_message.peer_type = sqlc.arg(peer_type)
  AND page_message.peer_id = sqlc.arg(peer_id)
  AND page_message.deleted = false
ORDER BY page_message.local_id DESC
LIMIT sqlc.arg(lim)::int
OFFSET (SELECT skip FROM page_offset);

-- name: SetEditedText :exec
UPDATE messages SET message = $3, edit_date = now() WHERE owner_id = $1 AND local_id = $2;

-- SetDeleted marks one owner-local message deleted and reports how many rows
-- it changed. The `AND NOT deleted` predicate is the whole point: it makes the
-- write the authority on whether the row was already gone, so a caller decides
-- under the same serialisation as the update rather than off a `deleted` flag
-- read before the per-owner locks. A second delete of the same row — a repeat
-- self-delete, or a revoke walk resuming after a concurrent self-delete landed
-- on the copy it is about to touch — changes zero rows instead of silently
-- re-marking a row that is already deleted.
-- name: SetDeleted :execrows
UPDATE messages SET deleted = true WHERE owner_id = $1 AND local_id = $2 AND NOT deleted;

-- name: SearchMessages :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)
  AND peer_type = sqlc.arg(peer_type)
  AND peer_id = sqlc.arg(peer_id)
  AND deleted = false
  AND message_tsv @@ plainto_tsquery('simple', sqlc.arg(query))
  AND (sqlc.arg(offset_id)::bigint = 0 OR local_id < sqlc.arg(offset_id)::bigint)
ORDER BY local_id DESC
LIMIT sqlc.arg(lim)::int;

-- Filtered shared-media searches count and page only the caller's owned rows.
-- Chat membership is repeated in the predicate so a removal between the
-- handler's admission check and this read cannot expose retained chat copies.
-- Filter values are 1=document, 2=photo, 3=URL, 4=video, 5=GIF, 6=poll,
-- 7=round video or voice, 8=audio, and 9=photo or video. The two file-backed
-- tabs are split on files.media_kind, so a photo is counted and listed under
-- Photos only and a document under Files only.
-- Polls are matched only through the caller's own local message copy. File
-- subtypes are matched only while the stored body can be rendered. URL
-- detection runs in Postgres over one authorized peer's rows; the app does not
-- load a dialog history to classify links.
-- name: CountFilteredMessages :one
SELECT count(*)::bigint
FROM messages m
WHERE m.owner_id = sqlc.arg(owner_id)::bigint
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
  AND m.peer_type IN (1, 2)
  AND m.deleted = false
  AND (m.peer_type <> 2 OR EXISTS (
      SELECT 1 FROM chat_participants cp
      WHERE cp.chat_id = m.peer_id AND cp.user_id = m.owner_id
  ))
  AND CASE sqlc.arg(filter)::smallint
      WHEN 1 THEN m.file_id <> 0 AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
            AND f.media_kind = 'document'
      )
      WHEN 2 THEN m.file_id <> 0 AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
            AND f.media_kind = 'photo'
      )
      WHEN 3 THEN m.message ~* '(^|[^[:alnum:]_@])(([[:alpha:]][[:alnum:]+.-]*://|www[.])[^[:space:]]+|[[:alnum:]-]+[.][[:alpha:]]{2,}(:[0-9]{1,5})?(/[[:graph:]]*)?)' -- noqa: LT05
      WHEN 4 THEN m.file_id <> 0 AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_videos']::text[]
      )
      WHEN 5 THEN m.file_id <> 0 AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_gifs']::text[]
      )
      WHEN 6 THEN EXISTS (
          SELECT 1 FROM poll_message_copies pmc
          WHERE pmc.owner_id = m.owner_id AND pmc.local_id = m.local_id
      )
      WHEN 7 THEN m.file_id <> 0 AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
            AND f.subtype_rights && ARRAY['send_roundvideos', 'send_voices']::text[]
      )
      WHEN 8 THEN m.file_id <> 0 AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_audios']::text[]
      )
      WHEN 9 THEN m.file_id <> 0 AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
            AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
      )
      ELSE false
  END
  AND (sqlc.arg(query)::text = '' OR m.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query)));

-- name: SearchFilteredMessagesPage :many
WITH page AS (
    SELECT m.local_id
    FROM messages m
    WHERE m.owner_id = sqlc.arg(owner_id)::bigint
      AND m.peer_type = sqlc.arg(peer_type)::smallint
      AND m.peer_id = sqlc.arg(peer_id)::bigint
      AND m.peer_type IN (1, 2)
      AND m.deleted = false
      AND (m.peer_type <> 2 OR EXISTS (
          SELECT 1 FROM chat_participants cp
          WHERE cp.chat_id = m.peer_id AND cp.user_id = m.owner_id
      ))
      AND CASE sqlc.arg(filter)::smallint
          WHEN 1 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.media_kind = 'document'
          )
          WHEN 2 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.media_kind = 'photo'
          )
          WHEN 3 THEN m.message ~* '(^|[^[:alnum:]_@])(([[:alpha:]][[:alnum:]+.-]*://|www[.])[^[:space:]]+|[[:alnum:]-]+[.][[:alpha:]]{2,}(:[0-9]{1,5})?(/[[:graph:]]*)?)' -- noqa: LT05
          WHEN 4 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights @> ARRAY['send_videos']::text[]
          )
          WHEN 5 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights @> ARRAY['send_gifs']::text[]
          )
          WHEN 6 THEN EXISTS (
              SELECT 1 FROM poll_message_copies pmc
              WHERE pmc.owner_id = m.owner_id AND pmc.local_id = m.local_id
          )
          WHEN 7 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights && ARRAY['send_roundvideos', 'send_voices']::text[]
          )
          WHEN 8 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights @> ARRAY['send_audios']::text[]
          )
          WHEN 9 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
          )
          ELSE false
      END
      AND (sqlc.arg(query)::text = '' OR m.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query)))
      AND (sqlc.arg(offset_id)::bigint = 0 OR m.local_id < sqlc.arg(offset_id)::bigint)
      AND (sqlc.arg(max_id)::bigint <= 0 OR m.local_id < sqlc.arg(max_id)::bigint)
      AND (sqlc.arg(min_id)::bigint <= 0 OR m.local_id > sqlc.arg(min_id)::bigint)
    ORDER BY m.local_id DESC
    LIMIT sqlc.arg(lim)::int
    OFFSET GREATEST(0::bigint, sqlc.arg(add_offset)::bigint)
)
SELECT m.*
FROM messages m
JOIN page ON page.local_id = m.local_id
WHERE m.owner_id = sqlc.arg(owner_id)::bigint
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
ORDER BY m.local_id DESC;

-- SearchFilteredMessagesPageAround counts the filter-matched rows before the
-- anchor so negative add_offset can select a page around offset_id.
-- name: SearchFilteredMessagesPageAround :many
WITH matching AS MATERIALIZED (
    SELECT m.local_id
    FROM messages m
    WHERE m.owner_id = sqlc.arg(owner_id)::bigint
      AND m.peer_type = sqlc.arg(peer_type)::smallint
      AND m.peer_id = sqlc.arg(peer_id)::bigint
      AND m.peer_type IN (1, 2)
      AND m.deleted = false
      AND (m.peer_type <> 2 OR EXISTS (
          SELECT 1 FROM chat_participants cp
          WHERE cp.chat_id = m.peer_id AND cp.user_id = m.owner_id
      ))
      AND CASE sqlc.arg(filter)::smallint
          WHEN 1 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.media_kind = 'document'
          )
          WHEN 2 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.media_kind = 'photo'
          )
          WHEN 3 THEN m.message ~* '(^|[^[:alnum:]_@])(([[:alpha:]][[:alnum:]+.-]*://|www[.])[^[:space:]]+|[[:alnum:]-]+[.][[:alpha:]]{2,}(:[0-9]{1,5})?(/[[:graph:]]*)?)' -- noqa: LT05
          WHEN 4 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights @> ARRAY['send_videos']::text[]
          )
          WHEN 5 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights @> ARRAY['send_gifs']::text[]
          )
          WHEN 6 THEN EXISTS (
              SELECT 1 FROM poll_message_copies pmc
              WHERE pmc.owner_id = m.owner_id AND pmc.local_id = m.local_id
          )
          WHEN 7 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights && ARRAY['send_roundvideos', 'send_voices']::text[]
          )
          WHEN 8 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND f.subtype_rights @> ARRAY['send_audios']::text[]
          )
          WHEN 9 THEN m.file_id <> 0 AND EXISTS (
              SELECT 1 FROM files f WHERE f.id = m.file_id AND f.stored = true
                AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
          )
          ELSE false
      END
      AND (sqlc.arg(query)::text = '' OR m.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query)))
      AND (sqlc.arg(max_id)::bigint <= 0 OR m.local_id < sqlc.arg(max_id)::bigint)
      AND (sqlc.arg(min_id)::bigint <= 0 OR m.local_id > sqlc.arg(min_id)::bigint)
), page_offset AS (
    SELECT GREATEST(
        COUNT(*) FILTER (WHERE local_id >= sqlc.arg(offset_id)::bigint) + sqlc.arg(add_offset)::bigint,
        0::bigint
    ) AS skip
    FROM matching
), page AS (
    SELECT local_id FROM matching
    ORDER BY local_id DESC
    LIMIT sqlc.arg(lim)::int
    OFFSET (SELECT skip FROM page_offset)
)
SELECT m.*
FROM messages m
JOIN page ON page.local_id = m.local_id
WHERE m.owner_id = sqlc.arg(owner_id)::bigint
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
ORDER BY m.local_id DESC;
