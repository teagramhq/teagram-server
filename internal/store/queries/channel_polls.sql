-- name: InsertChannelPollMessage :execrows
INSERT INTO channel_poll_messages (channel_id, local_id, poll_id)
VALUES ($1, $2, $3)
ON CONFLICT (channel_id, local_id) DO NOTHING;

-- name: PollByChannelMessage :one
SELECT p.*
FROM polls p
JOIN channel_poll_messages cpm ON cpm.poll_id = p.id
JOIN channel_messages cm ON cm.channel_id = cpm.channel_id AND cm.local_id = cpm.local_id
WHERE cpm.channel_id = $1
  AND cpm.local_id = $2
  AND cm.deleted = false;

-- name: ChannelPollMessageLocalIDs :many
SELECT local_id
FROM channel_poll_messages
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND local_id = ANY(sqlc.arg(local_ids)::bigint[])
ORDER BY local_id;

-- name: ChannelPollRecipients :many
SELECT user_id
FROM channel_participants
WHERE channel_id = $1
  AND (banned_until IS NULL OR banned_until <= clock_timestamp())
ORDER BY user_id;

-- name: PollChannelMessageForViewer :one
SELECT cpm.channel_id, cpm.local_id
FROM channel_poll_messages cpm
JOIN channel_participants cp ON cp.channel_id = cpm.channel_id
WHERE cpm.poll_id = $1
  AND cp.user_id = $2
  AND (cp.banned_until IS NULL OR cp.banned_until <= clock_timestamp());
