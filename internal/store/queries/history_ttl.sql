-- name: UserHistoryTTL :one
SELECT COALESCE((
    SELECT ttl_period FROM user_dialog_history_ttls
    WHERE user_low_id = LEAST(sqlc.arg(user_id)::bigint, sqlc.arg(peer_id)::bigint)
      AND user_high_id = GREATEST(sqlc.arg(user_id)::bigint, sqlc.arg(peer_id)::bigint)
), 0)::integer AS ttl_period;

-- name: SetUserHistoryTTL :exec
INSERT INTO user_dialog_history_ttls (user_low_id, user_high_id, ttl_period)
VALUES (LEAST(sqlc.arg(user_id)::bigint, sqlc.arg(peer_id)::bigint),
        GREATEST(sqlc.arg(user_id)::bigint, sqlc.arg(peer_id)::bigint),
        sqlc.arg(ttl_period)::integer)
ON CONFLICT (user_low_id, user_high_id)
DO UPDATE SET ttl_period = EXCLUDED.ttl_period;

-- name: SetChatHistoryTTL :one
UPDATE chats SET ttl_period = sqlc.arg(ttl_period)::integer, version = version + 1
WHERE id = sqlc.arg(chat_id)::bigint
RETURNING *;

-- name: SetChannelHistoryTTL :one
UPDATE channels SET ttl_period = sqlc.arg(ttl_period)::integer, version = version + 1
WHERE id = sqlc.arg(channel_id)::bigint
RETURNING *;

-- name: InsertChannelHistoryTTLChangeMessage :exec
INSERT INTO channel_messages (channel_id, local_id, from_id, message, action_type, ttl_period)
VALUES (sqlc.arg(channel_id)::bigint, sqlc.arg(local_id)::bigint, sqlc.arg(from_id)::bigint,
        '', 3, sqlc.arg(ttl_period)::integer);

-- ExpiredMessageCopyCandidates reads IDs without row locks. The caller first
-- takes the sorted per-owner advisory locks, then re-reads and locks still
-- expired rows before emitting delete events.
-- name: ExpiredMessageCopyCandidates :many
SELECT owner_id, local_id
FROM messages
WHERE deleted = false
  AND expires_at <= statement_timestamp()
ORDER BY expires_at, owner_id, local_id
LIMIT sqlc.arg(lim)::int;

-- ExpiredMessageCopyRowsForDelete rechecks eligibility after the owner locks
-- and takes message row locks in a stable order.
-- name: ExpiredMessageCopyRowsForDelete :many
SELECT message.owner_id, message.local_id, message.peer_type, message.peer_id
FROM messages AS message
WHERE message.owner_id = sqlc.arg(owner_id)::bigint
  AND message.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND message.deleted = false
  AND message.expires_at <= statement_timestamp()
ORDER BY message.local_id
FOR UPDATE OF message;
