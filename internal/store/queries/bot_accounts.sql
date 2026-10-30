-- name: CreateBotUser :one
INSERT INTO users (phone, login_mode, first_name, last_name, kind)
VALUES (NULL, 'username', sqlc.arg(first_name), '', 'bot')
RETURNING id;

-- name: InsertBotToken :exec
INSERT INTO bot_tokens (user_id, token_hash)
VALUES (sqlc.arg(user_id), sqlc.arg(token_hash));

-- name: ActiveBotTokenHash :one
SELECT token_hash
FROM bot_tokens
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- name: ActiveBotTokenState :one
SELECT token_hash, update_offset
FROM bot_tokens
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- StartBotUpdatePoll atomically claims a new generation and wakes existing
-- polls on every replica through the same update notification channel.
-- name: StartBotUpdatePoll :one
WITH claimed AS (
    UPDATE bot_tokens
    SET poll_generation = poll_generation + 1
    WHERE user_id = sqlc.arg(user_id)
      AND revoked_at IS NULL
      AND token_hash = sqlc.arg(token_hash)
    RETURNING user_id, update_offset, poll_generation
), notified AS (
    SELECT pg_notify('tg_updates', user_id::text)
    FROM claimed
)
SELECT claimed.update_offset, claimed.poll_generation
FROM claimed
CROSS JOIN notified;

-- name: ActiveBotPollGeneration :one
SELECT token_hash, poll_generation
FROM bot_tokens
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- name: FinishBotUpdatePoll :one
UPDATE bot_tokens
SET update_offset = GREATEST(update_offset, sqlc.arg(next_offset)::bigint)
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL
  AND token_hash = sqlc.arg(token_hash)
  AND poll_generation = sqlc.arg(poll_generation)::bigint
RETURNING update_offset;

-- BotMessageEvents returns a bounded event window, the retained incoming text
-- messages in that window, and the exclusive offset after the last scanned pts.
-- name: BotMessageEvents :many
WITH event_window AS MATERIALIZED (
    SELECT e.owner_id, e.pts, e.type, e.local_id
    FROM message_events AS e
    WHERE e.owner_id = sqlc.arg(owner_id)
      AND e.pts >= sqlc.arg(from_pts)::bigint
    ORDER BY e.pts
    LIMIT sqlc.arg(lim)::int
), bounded_events AS (
    SELECT e.owner_id,
           e.pts,
           e.type,
           e.local_id,
           (MAX(e.pts) OVER () + 1::bigint)::bigint AS next_offset
    FROM event_window AS e
)
SELECT e.pts,
       e.type,
       e.local_id,
       e.next_offset,
       COALESCE(sender.id, 0) > 0 AS has_human_sender,
       COALESCE(m.owner_id, 0)::bigint AS message_owner_id,
       COALESCE(m.local_id, 0)::bigint AS message_local_id,
       COALESCE(m.peer_type, 0)::smallint AS message_peer_type,
       COALESCE(m.peer_id, 0)::bigint AS message_peer_id,
       COALESCE(m.from_id, 0)::bigint AS message_from_id,
       COALESCE(m.date, 'epoch'::timestamptz) AS message_date,
       COALESCE(m.message, '')::text AS message_text,
       COALESCE(m.out, false) AS message_out,
       COALESCE(m.deleted, false) AS message_deleted,
       COALESCE(m.action_type, 0)::smallint AS message_action_type,
       COALESCE(m.file_id, 0)::bigint AS message_file_id,
       m.sticker_document_id AS message_sticker_document_id,
       COALESCE(sender.id, 0)::bigint AS sender_id,
       COALESCE(sender.kind, 'human')::text AS sender_kind,
       COALESCE(sender.first_name, '')::text AS sender_first_name,
       COALESCE(sender.last_name, '')::text AS sender_last_name,
       sender_username.handle AS sender_username
FROM bounded_events AS e
LEFT JOIN messages AS m
  ON m.owner_id = e.owner_id
 AND m.local_id = e.local_id
 AND e.type = 1
 AND m.peer_type = 1
 AND m.peer_id = m.from_id
 AND m.out = false
 AND m.deleted = false
 AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
 AND m.action_type = 0
 AND m.message <> ''
 AND m.file_id = 0
 AND m.sticker_document_id IS NULL
 AND NOT EXISTS (
     SELECT 1 FROM poll_message_copies AS p
     WHERE p.owner_id = m.owner_id AND p.local_id = m.local_id
 )
LEFT JOIN users AS sender
  ON sender.id = m.from_id
 AND sender.kind = 'human'
LEFT JOIN usernames AS sender_username
  ON sender_username.owner_type = 'user'
 AND sender_username.owner_id = sender.id
ORDER BY e.pts
;

-- name: RotateBotToken :execrows
UPDATE bot_tokens
SET token_hash = sqlc.arg(token_hash),
    issued_at = clock_timestamp(),
    revoked_at = NULL
WHERE user_id = sqlc.arg(user_id);

-- name: RevokeBotToken :execrows
UPDATE bot_tokens
SET token_hash = NULL,
    revoked_at = clock_timestamp()
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- name: UserKindByID :one
SELECT kind
FROM users
WHERE id = sqlc.arg(user_id);
