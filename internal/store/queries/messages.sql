-- name: InsertMessage :exec
INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out, random_id, peer_local_id,
                      fanout_id, action_type, action_user_id, file_id, sticker_document_id, reply_to_msg_id,
                      fwd_from_id, fwd_date, fwd_channel_id, fwd_channel_post,
                      fwd_post_author, reply_to_trusted, ttl_period, entities)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
        NULLIF(sqlc.arg(entities)::text, '')::jsonb);

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
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
  AND action_type = 0;

-- name: MessageByOwnerLocal :one
SELECT * FROM messages WHERE owner_id = $1 AND local_id = $2
  AND (expires_at IS NULL OR expires_at > statement_timestamp());

-- name: MessageByRandomID :one
SELECT * FROM messages
WHERE owner_id = $1 AND random_id = $2 AND random_id <> 0
  AND (expires_at IS NULL OR expires_at > statement_timestamp());

-- MessageRandomIDExpired reports an expired dedup token without loading any
-- message content, so callers can reject the retry without replaying it.
-- name: MessageRandomIDExpired :one
SELECT EXISTS (
    SELECT 1 FROM messages
    WHERE owner_id = sqlc.arg(owner_id)::bigint
      AND random_id = sqlc.arg(random_id)::bigint
      AND random_id <> 0
      AND expires_at <= statement_timestamp()
);

-- BotHasStartedPrivateChat is evaluated inside the same locked transaction as
-- a Bot API send. Only an incoming direct message from the target user proves
-- that the user started this bot; bot outbox and non-user peer rows do not.
-- name: BotHasStartedPrivateChat :one
SELECT EXISTS (
    SELECT 1 FROM messages
    WHERE owner_id = sqlc.arg(bot_id)::bigint
      AND peer_type = sqlc.arg(peer_type)::smallint
      AND peer_id = sqlc.arg(user_id)::bigint
      AND from_id = sqlc.arg(user_id)::bigint
      AND out = false
);

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

-- ChatMessageCopiesForDeletion returns active copies owned by participants
-- captured under the chat row lock. Former members are excluded so their
-- frozen copies retain the existing group-revoke behavior.
-- name: ChatMessageCopiesForDeletion :many
SELECT * FROM messages
WHERE peer_type = 2
  AND peer_id = sqlc.arg(chat_id)::bigint
  AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND deleted = false
ORDER BY owner_id, local_id;

-- name: HistoryPage :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)
  AND peer_type = sqlc.arg(peer_type)
  AND peer_id = sqlc.arg(peer_id)
  AND deleted = false
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
  AND (sqlc.arg(offset_id)::bigint = 0 OR local_id < sqlc.arg(offset_id)::bigint)
ORDER BY local_id DESC
LIMIT sqlc.arg(lim)::int
OFFSET GREATEST(0::bigint, sqlc.arg(add_offset)::bigint);

-- HistoryPageByDate keeps date jumps on an ordered peer/date index.
-- name: HistoryPageByDate :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)
  AND peer_type = sqlc.arg(peer_type)
  AND peer_id = sqlc.arg(peer_id)
  AND deleted = false
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
  AND date < to_timestamp(sqlc.arg(offset_date)::bigint::double precision)
ORDER BY date DESC, local_id DESC
LIMIT sqlc.arg(lim)::int
OFFSET GREATEST(0::bigint, sqlc.arg(add_offset)::bigint);

-- HistoryPageByDateAround shifts negative add_offset across the date anchor
-- using bounded reads from messages_owner_peer_date_idx without rereading
-- the shifted messages for the returned page. The materialized window is
-- capped at the page size even when add_offset is larger.
-- name: HistoryPageByDateAround :many
WITH shifted_window AS MATERIALIZED (
    SELECT messages.*
    FROM messages
    WHERE messages.owner_id = sqlc.arg(owner_id)
      AND messages.peer_type = sqlc.arg(peer_type)
      AND messages.peer_id = sqlc.arg(peer_id)
      AND messages.deleted = false
      AND (messages.expires_at IS NULL OR messages.expires_at > statement_timestamp())
      AND messages.date >= to_timestamp(sqlc.arg(offset_date)::bigint::double precision)
    ORDER BY messages.date ASC, messages.local_id ASC
    LIMIT LEAST(-sqlc.arg(add_offset)::bigint, sqlc.arg(lim)::bigint)::int
    OFFSET GREATEST(
        0::bigint,
        -sqlc.arg(add_offset)::bigint - sqlc.arg(lim)::bigint
    )::int
),
window_state AS MATERIALIZED (
    SELECT count(*)::bigint AS row_count
    FROM shifted_window
),
shifted_page AS (
    SELECT window_message.*
    FROM shifted_window AS window_message
    WHERE (SELECT row_count FROM window_state) =
          LEAST(-sqlc.arg(add_offset)::bigint, sqlc.arg(lim)::bigint)
    ORDER BY window_message.date DESC, window_message.local_id DESC
    LIMIT sqlc.arg(lim)::int
),
older_page AS (
    SELECT page_message.*
    FROM messages AS page_message
    WHERE page_message.owner_id = sqlc.arg(owner_id)
      AND page_message.peer_type = sqlc.arg(peer_type)
      AND page_message.peer_id = sqlc.arg(peer_id)
      AND page_message.deleted = false
      AND (page_message.expires_at IS NULL OR page_message.expires_at > statement_timestamp())
      AND (page_message.date, page_message.local_id) < (
          SELECT window_message.date, window_message.local_id
          FROM shifted_window AS window_message
          ORDER BY window_message.date ASC, window_message.local_id ASC
          LIMIT 1
      )
      AND (SELECT row_count FROM window_state) =
          LEAST(-sqlc.arg(add_offset)::bigint, sqlc.arg(lim)::bigint)
    ORDER BY page_message.date DESC, page_message.local_id DESC
    LIMIT GREATEST(
        0::bigint,
        sqlc.arg(lim)::bigint + sqlc.arg(add_offset)::bigint
    )::int
),
latest_page AS (
    SELECT page_message.*
    FROM messages AS page_message
    WHERE page_message.owner_id = sqlc.arg(owner_id)
      AND page_message.peer_type = sqlc.arg(peer_type)
      AND page_message.peer_id = sqlc.arg(peer_id)
      AND page_message.deleted = false
      AND (page_message.expires_at IS NULL OR page_message.expires_at > statement_timestamp())
      AND (SELECT row_count FROM window_state) <
          LEAST(-sqlc.arg(add_offset)::bigint, sqlc.arg(lim)::bigint)
    ORDER BY page_message.date DESC, page_message.local_id DESC
    LIMIT sqlc.arg(lim)::int
)
SELECT page_message.*
FROM (
    SELECT * FROM shifted_page
    UNION ALL
    SELECT * FROM older_page
    UNION ALL
    SELECT * FROM latest_page
) AS page_message
ORDER BY page_message.date DESC, page_message.local_id DESC
LIMIT sqlc.arg(lim)::int;

-- HistoryPageAround shifts a negative add_offset with a bounded ascending
-- index walk from offset_id before reading the requested page.
-- name: HistoryPageAround :many
WITH shifted_window AS MATERIALIZED (
    SELECT local_id
    FROM messages
    WHERE owner_id = sqlc.arg(owner_id)
      AND peer_type = sqlc.arg(peer_type)
      AND peer_id = sqlc.arg(peer_id)
      AND deleted = false
      AND (expires_at IS NULL OR expires_at > statement_timestamp())
      AND local_id >= sqlc.arg(offset_id)::bigint
    ORDER BY local_id ASC
    LIMIT GREATEST(0::bigint, -sqlc.arg(add_offset)::bigint)
)
SELECT page_message.* FROM messages AS page_message
WHERE page_message.owner_id = sqlc.arg(owner_id)
  AND page_message.peer_type = sqlc.arg(peer_type)
  AND page_message.peer_id = sqlc.arg(peer_id)
  AND page_message.deleted = false
  AND (page_message.expires_at IS NULL OR page_message.expires_at > statement_timestamp())
  AND page_message.local_id <= COALESCE(
      (SELECT max(local_id) FROM shifted_window
       HAVING count(*) = -sqlc.arg(add_offset)::bigint),
      9223372036854775807::bigint
  )
ORDER BY page_message.local_id DESC
LIMIT sqlc.arg(lim)::int;

-- name: SetEditedText :exec
UPDATE messages SET message = $3, edit_date = now() WHERE owner_id = $1 AND local_id = $2;

-- name: SetEditedTextWithEntities :exec
UPDATE messages
SET message = $3, edit_date = now(), entities = NULLIF(sqlc.arg(entities)::text, '')::jsonb
WHERE owner_id = $1 AND local_id = $2;

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

-- DeleteHistoryBatch is the sole entry into one bounded history-delete batch:
-- every candidate belongs to the caller and the requested peer, is still live,
-- and basic-group service messages are never selected.
-- name: DeleteHistoryBatch :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND peer_type = sqlc.arg(peer_type)::smallint
  AND peer_id = sqlc.arg(peer_id)::bigint
  AND (sqlc.arg(max_id)::bigint = 0 OR local_id <= sqlc.arg(max_id)::bigint)
  AND deleted = false
  AND (peer_type <> 2 OR action_type = 0)
ORDER BY local_id DESC
LIMIT sqlc.arg(lim)::int;

-- DeleteHistoryRemaining counts only rows that another bounded call may select.
-- name: DeleteHistoryRemaining :one
SELECT count(*)::bigint
FROM messages
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND peer_type = sqlc.arg(peer_type)::smallint
  AND peer_id = sqlc.arg(peer_id)::bigint
  AND (sqlc.arg(max_id)::bigint = 0 OR local_id <= sqlc.arg(max_id)::bigint)
  AND deleted = false
  AND (peer_type <> 2 OR action_type = 0);

-- name: SearchMessages :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)
  AND peer_type = sqlc.arg(peer_type)
  AND peer_id = sqlc.arg(peer_id)
  AND deleted = false
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
  AND message_tsv @@ plainto_tsquery('simple', sqlc.arg(query))
  AND (sqlc.arg(offset_id)::bigint = 0 OR local_id < sqlc.arg(offset_id)::bigint)
ORDER BY local_id DESC
LIMIT sqlc.arg(lim)::int;

-- MessageSearchDialogReady checks whether the postings for one dialog are
-- initialized. Keep this separate from the page query so PostgreSQL never
-- plans and scans the legacy history path for a ready dialog.
-- name: MessageSearchDialogReady :one
SELECT NOT EXISTS (
    SELECT 1
    FROM message_search_unready_dialogs
    WHERE owner_id = sqlc.arg(owner_id)::bigint
      AND peer_type = sqlc.arg(peer_type)::smallint
      AND peer_id = sqlc.arg(peer_id)::bigint
);

-- SearchMessagesByPostings reads only the ordered lexeme postings for one
-- ready dialog. The per-dialog posting counts choose the rarest query lexeme
-- before the ordered walk; the tsquery check preserves full-query semantics.
-- name: SearchMessagesByPostings :many
WITH parsed_query AS MATERIALIZED (
    SELECT plainto_tsquery('simple', sqlc.arg(query)::text) AS tsquery,
           tsvector_to_array(to_tsvector('simple', sqlc.arg(query)::text)) AS lexemes
),
query_lexemes AS (
    SELECT parsed.tsquery, term.lexeme
    FROM parsed_query AS parsed
    CROSS JOIN LATERAL unnest(parsed.lexemes) AS term(lexeme)
),
selected_query AS MATERIALIZED (
    SELECT ql.tsquery, ql.lexeme
    FROM query_lexemes AS ql
    LEFT JOIN message_search_posting_counts AS counts
      ON counts.owner_id = sqlc.arg(owner_id)::bigint
     AND counts.peer_type = sqlc.arg(peer_type)::smallint
     AND counts.peer_id = sqlc.arg(peer_id)::bigint
     AND counts.lexeme = ql.lexeme
    ORDER BY COALESCE(counts.posting_count, 0), ql.lexeme
    LIMIT 1
)
SELECT m.*
FROM selected_query AS q
CROSS JOIN LATERAL (
    SELECT m.*
    FROM message_search_postings AS p
    JOIN messages AS m
      ON m.owner_id = p.owner_id
     AND m.local_id = p.local_id
     AND m.peer_type = p.peer_type
     AND m.peer_id = p.peer_id
    WHERE p.owner_id = sqlc.arg(owner_id)::bigint
      AND p.peer_type = sqlc.arg(peer_type)::smallint
      AND p.peer_id = sqlc.arg(peer_id)::bigint
      AND p.lexeme = q.lexeme
      AND m.deleted = false
      AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
      AND m.message_tsv @@ q.tsquery
      AND (sqlc.arg(offset_id)::bigint = 0 OR p.local_id < sqlc.arg(offset_id)::bigint)
    ORDER BY p.local_id DESC
    LIMIT sqlc.arg(lim)::int
) AS m
ORDER BY m.local_id DESC;

-- SearchResultsCalendar is the date picker's per-day photo/video preview. Keep
-- file_id <> 0 as a top-level predicate so the partial messages_file_idx can
-- serve a large text-only history without scanning its message rows.
-- Count the full filter here; offset cursors apply only to periods and messages.
-- name: SearchResultsCalendarCount :one
SELECT COUNT(*)::bigint
FROM messages AS m
WHERE m.owner_id = sqlc.arg(owner_id)::bigint
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
  AND m.peer_type IN (1, 2)
  AND m.deleted = false
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
  AND m.file_id <> 0
  AND (m.peer_type <> 2 OR EXISTS (
      SELECT 1 FROM chat_participants cp
      WHERE cp.chat_id = m.peer_id AND cp.user_id = m.owner_id
  ))
  AND EXISTS (
      SELECT 1 FROM files f
      WHERE f.id = m.file_id AND f.stored = true
        AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
  );

-- name: SearchResultsCalendarPeriods :many
SELECT EXTRACT(EPOCH FROM date_trunc('day', m.date AT TIME ZONE 'UTC'))::bigint AS date,
       MIN(m.local_id)::bigint AS min_msg_id,
       MAX(m.local_id)::bigint AS max_msg_id,
       COUNT(*)::int AS count
FROM messages AS m
WHERE m.owner_id = sqlc.arg(owner_id)::bigint
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
  AND m.peer_type IN (1, 2)
  AND m.deleted = false
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
  AND m.file_id <> 0
  AND (sqlc.arg(offset_date)::bigint = 0 OR m.date <= to_timestamp(sqlc.arg(offset_date)::bigint::double precision))
  AND (sqlc.arg(offset_id)::bigint = 0 OR m.local_id < sqlc.arg(offset_id)::bigint)
  AND (m.peer_type <> 2 OR EXISTS (
      SELECT 1 FROM chat_participants cp
      WHERE cp.chat_id = m.peer_id AND cp.user_id = m.owner_id
  ))
  AND EXISTS (
      SELECT 1 FROM files f
      WHERE f.id = m.file_id AND f.stored = true
        AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
  )
GROUP BY date_trunc('day', m.date AT TIME ZONE 'UTC')
ORDER BY date DESC;

-- name: SearchResultsCalendarPage :many
SELECT m.*
FROM messages AS m
WHERE m.owner_id = sqlc.arg(owner_id)::bigint
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
  AND m.peer_type IN (1, 2)
  AND m.deleted = false
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
  AND m.file_id <> 0
  AND (sqlc.arg(offset_date)::bigint = 0 OR m.date <= to_timestamp(sqlc.arg(offset_date)::bigint::double precision))
  AND (sqlc.arg(offset_id)::bigint = 0 OR m.local_id < sqlc.arg(offset_id)::bigint)
  AND (m.peer_type <> 2 OR EXISTS (
      SELECT 1 FROM chat_participants cp
      WHERE cp.chat_id = m.peer_id AND cp.user_id = m.owner_id
  ))
  AND EXISTS (
      SELECT 1 FROM files f
      WHERE f.id = m.file_id AND f.stored = true
        AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
  )
ORDER BY m.date DESC, m.local_id DESC
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
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
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
      AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
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
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
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
      AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
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
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
ORDER BY m.local_id DESC;
