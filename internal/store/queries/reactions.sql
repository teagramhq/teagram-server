-- name: UpsertReaction :exec
INSERT INTO message_reactions (owner_id, local_id, reactor_id, reaction)
VALUES ($1, $2, $3, $4)
ON CONFLICT (owner_id, local_id, reactor_id)
DO UPDATE SET reaction = $4, created_at = statement_timestamp();

-- name: DeleteReaction :exec
DELETE FROM message_reactions
WHERE owner_id = $1 AND local_id = $2 AND reactor_id = $3;

-- name: ReactionsByMessage :many
SELECT * FROM message_reactions
WHERE message_reactions.owner_id = $1 AND message_reactions.local_id = $2
  AND EXISTS (
      SELECT 1 FROM messages m
      WHERE m.owner_id = message_reactions.owner_id AND m.local_id = message_reactions.local_id
        AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
  )
ORDER BY reactor_id;

-- name: ReactionsByMessages :many
SELECT * FROM message_reactions
WHERE message_reactions.owner_id = sqlc.arg(owner_id)
  AND message_reactions.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND EXISTS (
      SELECT 1 FROM messages m
      WHERE m.owner_id = message_reactions.owner_id AND m.local_id = message_reactions.local_id
        AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
  )
ORDER BY local_id, reactor_id;

-- name: MessageReactionListForOwner :many
WITH visible_message AS MATERIALIZED (
    SELECT m.owner_id, m.local_id
    FROM messages AS m
    JOIN chat_participants AS participant
      ON participant.chat_id = m.peer_id
     AND participant.user_id = sqlc.arg(owner_id)::bigint
    WHERE m.owner_id = sqlc.arg(owner_id)::bigint
      AND m.local_id = sqlc.arg(local_id)::bigint
      AND m.peer_type = 2
      AND m.peer_id = sqlc.arg(chat_id)::bigint
      AND m.deleted = false
      AND m.action_type = 0
      AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
), matching AS MATERIALIZED (
    SELECT reaction.reactor_id, reaction.reaction, reaction.created_at
    FROM message_reactions AS reaction
    JOIN visible_message AS message
      ON message.owner_id = reaction.owner_id
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

-- name: MessagesByOwnerLocalIDs :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)
  AND local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND (expires_at IS NULL OR expires_at > statement_timestamp());
