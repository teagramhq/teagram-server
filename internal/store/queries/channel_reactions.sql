-- LockChannelForReaction takes a shared lock on a live channel. It is
-- compatible with concurrent reactions and serializes them with channel
-- deletion and settings/rights mutations.
-- name: LockChannelForReaction :one
SELECT id FROM channels
WHERE id = sqlc.arg(channel_id)::bigint AND deleted_at IS NULL
FOR SHARE;

-- ChannelReactionParticipantForShare locks membership, ban state, and the
-- history floor after LockChannelForReaction. Reaction writers then lock the
-- post row, matching the channel-message tombstone order without channel_state.
-- name: ChannelReactionParticipantForShare :one
SELECT * FROM channel_participants
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND user_id = sqlc.arg(user_id)::bigint
FOR SHARE;

-- The caller already holds the participant row FOR SHARE and passes its locked
-- available_min_id. A miss deliberately combines absent, hidden, deleted,
-- expired, and non-post message IDs into one result.
-- name: ChannelReactionMessageForShare :one
SELECT cm.local_id
FROM channel_messages AS cm
WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
  AND cm.local_id = sqlc.arg(local_id)::bigint
  AND cm.local_id > sqlc.arg(available_min_id)::bigint
  AND cm.action_type = 0
  AND cm.deleted = false
  AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
FOR SHARE OF cm;

-- ChannelAvailableReactionsByIDs reads only explicit channel settings. A
-- missing row inherits all standard reactions.
-- name: ChannelAvailableReactionsByIDs :many
SELECT channel_id, mode, reactions
FROM channel_available_reactions
WHERE channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
ORDER BY channel_id;

-- name: SetChannelAvailableReactions :exec
INSERT INTO channel_available_reactions (channel_id, mode, reactions)
VALUES (sqlc.arg(channel_id)::bigint, sqlc.arg(mode)::text, sqlc.arg(reactions)::text[])
ON CONFLICT (channel_id)
DO UPDATE SET mode = EXCLUDED.mode, reactions = EXCLUDED.reactions;

-- name: UpsertChannelMessageReaction :exec
INSERT INTO channel_message_reactions (channel_id, local_id, reactor_id, reaction)
VALUES (sqlc.arg(channel_id)::bigint, sqlc.arg(local_id)::bigint,
        sqlc.arg(reactor_id)::bigint, sqlc.arg(reaction)::text)
ON CONFLICT (channel_id, local_id, reactor_id)
DO UPDATE SET reaction = EXCLUDED.reaction, created_at = statement_timestamp();

-- name: DeleteChannelMessageReaction :exec
DELETE FROM channel_message_reactions
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND local_id = sqlc.arg(local_id)::bigint
  AND reactor_id = sqlc.arg(reactor_id)::bigint;

-- name: BumpChannelVersionForAvailableReactions :execrows
UPDATE channels SET version = version + 1
WHERE id = sqlc.arg(channel_id)::bigint AND deleted_at IS NULL;

-- The aggregate query never reads reactor_id. Its join is a backstop against
-- stale reactions appearing on deleted or expired posts.
-- name: ChannelReactionCountsByMessageIDs :many
SELECT reaction.channel_id, reaction.local_id, reaction.reaction,
       count(*)::bigint AS reaction_count
FROM channel_message_reactions AS reaction
JOIN channel_messages AS message
  ON message.channel_id = reaction.channel_id
 AND message.local_id = reaction.local_id
JOIN channels AS channel
  ON channel.id = message.channel_id
WHERE reaction.channel_id = sqlc.arg(channel_id)::bigint
  AND reaction.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND message.action_type = 0
  AND message.deleted = false
  AND (message.expires_at IS NULL OR message.expires_at > statement_timestamp())
  AND channel.deleted_at IS NULL
GROUP BY reaction.channel_id, reaction.local_id, reaction.reaction
ORDER BY reaction.local_id, reaction.reaction;

-- Caller choice is read separately from the aggregate, keyed by the caller's
-- own identity; no other reactor identity leaves the store.
-- name: ChannelOwnReactionsByMessageIDs :many
SELECT reaction.local_id, reaction.reaction
FROM channel_message_reactions AS reaction
JOIN channel_messages AS message
  ON message.channel_id = reaction.channel_id
 AND message.local_id = reaction.local_id
JOIN channels AS channel ON channel.id = message.channel_id
WHERE reaction.channel_id = sqlc.arg(channel_id)::bigint
  AND reaction.reactor_id = sqlc.arg(reactor_id)::bigint
  AND reaction.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND message.action_type = 0
  AND message.deleted = false
  AND (message.expires_at IS NULL OR message.expires_at > statement_timestamp())
  AND channel.deleted_at IS NULL
ORDER BY reaction.local_id;

-- Soft-deleted channel posts do not trigger FK cascades, so every tombstone
-- path deletes reaction rows explicitly in its transaction.
-- name: DeleteChannelMessageReactions :exec
DELETE FROM channel_message_reactions
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND local_id = ANY(sqlc.arg(local_ids)::bigint[]);

-- name: ChannelMessageReactionListForMember :many
WITH visible_message AS MATERIALIZED (
    SELECT message.channel_id, message.local_id
    FROM channel_messages AS message
    JOIN channels AS channel ON channel.id = message.channel_id
    JOIN channel_participants AS participant
      ON participant.channel_id = message.channel_id
     AND participant.user_id = sqlc.arg(user_id)::bigint
    WHERE message.channel_id = sqlc.arg(channel_id)::bigint
      AND message.local_id = sqlc.arg(local_id)::bigint
      AND (participant.banned_until IS NULL OR participant.banned_until <= statement_timestamp())
      AND message.local_id > participant.available_min_id
      AND message.deleted = false
      AND message.action_type = 0
      AND (message.expires_at IS NULL OR message.expires_at > statement_timestamp())
      AND channel.deleted_at IS NULL
      AND channel.megagroup = true
), matching AS MATERIALIZED (
    SELECT reaction.reactor_id, reaction.reaction, reaction.created_at
    FROM channel_message_reactions AS reaction
    JOIN visible_message AS message
      ON message.channel_id = reaction.channel_id
     AND message.local_id = reaction.local_id
    WHERE sqlc.arg(reaction)::text = ''
       OR reaction.reaction = sqlc.arg(reaction)::text
), page AS MATERIALIZED (
    SELECT reactor_id, reaction, created_at
    FROM matching
    WHERE reactor_id > sqlc.arg(offset_reactor_id)::bigint
    ORDER BY reactor_id
    LIMIT sqlc.arg(page_size)::bigint
)
SELECT EXISTS (SELECT 1 FROM visible_message) AS message_visible,
       (SELECT count(*)::bigint FROM matching) AS reaction_count,
       page.reactor_id,
       page.reaction,
       page.created_at
FROM (SELECT 1) AS requested
LEFT JOIN page ON true
ORDER BY page.reactor_id NULLS LAST;
