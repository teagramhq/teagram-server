-- name: CloudDraftTimestamp :one
SELECT clock_timestamp()::timestamptz AS updated_at;

-- name: UpsertCloudDraft :exec
INSERT INTO cloud_drafts (owner_id, peer_type, peer_id, message, no_webpage, reply_to_msg_id, updated_at)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, 0), $7)
ON CONFLICT (owner_id, peer_type, peer_id) DO UPDATE SET
    message = EXCLUDED.message,
    no_webpage = EXCLUDED.no_webpage,
    reply_to_msg_id = EXCLUDED.reply_to_msg_id,
    updated_at = EXCLUDED.updated_at;

-- name: DeleteCloudDraft :execrows
DELETE FROM cloud_drafts
WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3;

-- name: MarkCloudDraftChanged :exec
INSERT INTO cloud_draft_sync (owner_id, peer_type, peer_id, changed_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (owner_id, peer_type, peer_id) DO UPDATE
SET changed_at = EXCLUDED.changed_at;

-- name: CloudDraftUserDialogExists :one
SELECT EXISTS (
    SELECT 1 FROM dialogs
    WHERE owner_id = $1 AND peer_type = 1 AND peer_id = $2
);

-- CloudDraftsForPeers hydrates only the selected current dialogs, and rechecks
-- current group/channel membership in the same query that reads the private
-- text. A removed member's retained dialogs row cannot expose its draft.
-- name: CloudDraftsForPeers :many
WITH selected AS (
    SELECT peer_types.peer_type, peer_ids.peer_id
    FROM unnest(sqlc.arg(peer_types)::smallint[]) WITH ORDINALITY AS peer_types(peer_type, ord)
    JOIN unnest(sqlc.arg(peer_ids)::bigint[]) WITH ORDINALITY AS peer_ids(peer_id, ord) USING (ord)
)
SELECT draft.peer_type,
       draft.peer_id,
       draft.message,
       draft.no_webpage,
       CASE
           WHEN draft.reply_to_msg_id IS NULL THEN 0::bigint
           WHEN draft.peer_type = 3 AND EXISTS (
               SELECT 1 FROM channel_messages AS target
               WHERE target.channel_id = draft.peer_id
                 AND target.local_id = draft.reply_to_msg_id
                 AND target.deleted = false
                 AND target.action_type = 0
           ) THEN draft.reply_to_msg_id
           WHEN draft.peer_type <> 3 AND EXISTS (
               SELECT 1 FROM messages AS target
               WHERE target.owner_id = draft.owner_id
                 AND target.peer_type = draft.peer_type
                 AND target.peer_id = draft.peer_id
                 AND target.local_id = draft.reply_to_msg_id
                 AND target.deleted = false
                 AND target.action_type = 0
           ) THEN draft.reply_to_msg_id
           ELSE 0::bigint
       END AS reply_to_msg_id,
       draft.updated_at
FROM cloud_drafts AS draft
JOIN selected
  ON selected.peer_type = draft.peer_type AND selected.peer_id = draft.peer_id
WHERE draft.owner_id = sqlc.arg(owner_id)
  AND (
      (draft.peer_type = 1 AND EXISTS (
          SELECT 1 FROM dialogs AS dialog
          WHERE dialog.owner_id = draft.owner_id
            AND dialog.peer_type = draft.peer_type
            AND dialog.peer_id = draft.peer_id
      ))
      OR (draft.peer_type = 2 AND EXISTS (
          SELECT 1 FROM chat_participants AS participant
          WHERE participant.chat_id = draft.peer_id AND participant.user_id = draft.owner_id
      ))
      OR (draft.peer_type = 3 AND EXISTS (
          SELECT 1 FROM channel_participants AS participant
          WHERE participant.channel_id = draft.peer_id
            AND participant.user_id = draft.owner_id
            AND (participant.banned_until IS NULL OR participant.banned_until <= now())
      ))
  )
ORDER BY draft.peer_type, draft.peer_id;

-- CloudDraftStateForPeer delivers the latest owner-visible value or clear
-- tombstone after a peer-free NOTIFY. Access is checked with the same statement
-- as the state read so a removal that committed first suppresses delivery.
-- name: CloudDraftStateForPeer :one
SELECT changed.peer_type,
       changed.peer_id,
       changed.changed_at,
       (draft.owner_id IS NOT NULL) AS has_draft,
       COALESCE(draft.message, '') AS message,
       COALESCE(draft.no_webpage, false) AS no_webpage,
       CASE
           WHEN draft.reply_to_msg_id IS NULL THEN 0::bigint
           WHEN draft.peer_type = 3 AND EXISTS (
               SELECT 1 FROM channel_messages AS target
               WHERE target.channel_id = draft.peer_id
                 AND target.local_id = draft.reply_to_msg_id
                 AND target.deleted = false
                 AND target.action_type = 0
           ) THEN draft.reply_to_msg_id
           WHEN draft.peer_type <> 3 AND EXISTS (
               SELECT 1 FROM messages AS target
               WHERE target.owner_id = draft.owner_id
                 AND target.peer_type = draft.peer_type
                 AND target.peer_id = draft.peer_id
                 AND target.local_id = draft.reply_to_msg_id
                 AND target.deleted = false
                 AND target.action_type = 0
           ) THEN draft.reply_to_msg_id
           ELSE 0::bigint
       END AS reply_to_msg_id,
       COALESCE(draft.updated_at, changed.changed_at) AS updated_at
FROM cloud_draft_sync AS changed
LEFT JOIN cloud_drafts AS draft
  ON draft.owner_id = changed.owner_id
 AND draft.peer_type = changed.peer_type
 AND draft.peer_id = changed.peer_id
WHERE changed.owner_id = sqlc.arg(owner_id)
  AND changed.peer_type = sqlc.arg(peer_type)
  AND changed.peer_id = sqlc.arg(peer_id)
  AND (
      (changed.peer_type = 1 AND EXISTS (
          SELECT 1 FROM dialogs AS dialog
          WHERE dialog.owner_id = changed.owner_id
            AND dialog.peer_type = changed.peer_type
            AND dialog.peer_id = changed.peer_id
      ))
      OR (changed.peer_type = 2 AND EXISTS (
          SELECT 1 FROM chat_participants AS participant
          WHERE participant.chat_id = changed.peer_id AND participant.user_id = changed.owner_id
      ))
      OR (changed.peer_type = 3 AND EXISTS (
          SELECT 1 FROM channel_participants AS participant
          WHERE participant.channel_id = changed.peer_id
            AND participant.user_id = changed.owner_id
            AND (participant.banned_until IS NULL OR participant.banned_until <= now())
      ))
  );

-- CloudDraftChangesForOwnerSince is a separate non-PTS recovery stream. Each
-- change is bounded by the saveDraft account budget; the peer key carries no
-- text, and the current value is resolved at read time.
-- name: CloudDraftChangesForOwnerSince :many
SELECT changed.peer_type,
       changed.peer_id,
       changed.changed_at,
       (draft.owner_id IS NOT NULL) AS has_draft,
       COALESCE(draft.message, '') AS message,
       COALESCE(draft.no_webpage, false) AS no_webpage,
       CASE
           WHEN draft.reply_to_msg_id IS NULL THEN 0::bigint
           WHEN draft.peer_type = 3 AND EXISTS (
               SELECT 1 FROM channel_messages AS target
               WHERE target.channel_id = draft.peer_id
                 AND target.local_id = draft.reply_to_msg_id
                 AND target.deleted = false
                 AND target.action_type = 0
           ) THEN draft.reply_to_msg_id
           WHEN draft.peer_type <> 3 AND EXISTS (
               SELECT 1 FROM messages AS target
               WHERE target.owner_id = draft.owner_id
                 AND target.peer_type = draft.peer_type
                 AND target.peer_id = draft.peer_id
                 AND target.local_id = draft.reply_to_msg_id
                 AND target.deleted = false
                 AND target.action_type = 0
           ) THEN draft.reply_to_msg_id
           ELSE 0::bigint
       END AS reply_to_msg_id,
       COALESCE(draft.updated_at, changed.changed_at) AS updated_at
FROM cloud_draft_sync AS changed
LEFT JOIN cloud_drafts AS draft
  ON draft.owner_id = changed.owner_id
 AND draft.peer_type = changed.peer_type
 AND draft.peer_id = changed.peer_id
WHERE changed.owner_id = sqlc.arg(owner_id)
  AND changed.changed_at >= sqlc.arg(changed_since)
  AND (
      (changed.peer_type = 1 AND EXISTS (
          SELECT 1 FROM dialogs AS dialog
          WHERE dialog.owner_id = changed.owner_id
            AND dialog.peer_type = changed.peer_type
            AND dialog.peer_id = changed.peer_id
      ))
      OR (changed.peer_type = 2 AND EXISTS (
          SELECT 1 FROM chat_participants AS participant
          WHERE participant.chat_id = changed.peer_id AND participant.user_id = changed.owner_id
      ))
      OR (changed.peer_type = 3 AND EXISTS (
          SELECT 1 FROM channel_participants AS participant
          WHERE participant.channel_id = changed.peer_id
            AND participant.user_id = changed.owner_id
            AND (participant.banned_until IS NULL OR participant.banned_until <= now())
      ))
  )
ORDER BY changed.changed_at, changed.peer_type, changed.peer_id
LIMIT 500;
