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

-- BotMessageEvents returns only retained incoming private text-message events.
-- Other pts values remain gaps; the lower-bound query never depends on finding
-- a row at every offset.
-- name: BotMessageEvents :many
SELECT e.pts,
       e.type,
       e.local_id,
       m.owner_id AS message_owner_id,
       m.local_id AS message_local_id,
       m.peer_type AS message_peer_type,
       m.peer_id AS message_peer_id,
       m.from_id AS message_from_id,
       m.date AS message_date,
       m.message AS message_text,
       m.out AS message_out,
       m.deleted AS message_deleted,
       m.action_type AS message_action_type,
       m.file_id AS message_file_id,
       m.sticker_document_id AS message_sticker_document_id,
       sender.id AS sender_id,
       sender.kind AS sender_kind,
       sender.first_name AS sender_first_name,
       sender.last_name AS sender_last_name,
       sender_username.handle AS sender_username
FROM message_events AS e
JOIN messages AS m
  ON m.owner_id = e.owner_id
 AND m.local_id = e.local_id
JOIN users AS sender
  ON sender.id = m.from_id
 AND sender.kind = 'human'
LEFT JOIN usernames AS sender_username
  ON sender_username.owner_type = 'user'
 AND sender_username.owner_id = sender.id
WHERE e.owner_id = sqlc.arg(owner_id)
  AND e.pts >= sqlc.arg(from_pts)::bigint
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
ORDER BY e.pts
LIMIT sqlc.arg(lim)::int;

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
