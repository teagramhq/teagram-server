-- name: DialogUnreadMarkTimestamp :one
SELECT clock_timestamp()::timestamptz AS changed_at;

-- name: DialogUnreadMarkUserDialogExists :one
SELECT EXISTS (
    SELECT 1 FROM dialogs
    WHERE owner_id = $1 AND peer_type = 1 AND peer_id = $2
);

-- name: DialogUnreadMarkChatMember :one
SELECT EXISTS (
    SELECT 1 FROM chat_participants
    WHERE chat_id = $1 AND user_id = $2
);

-- name: DialogUnreadMarkChannelMember :one
SELECT EXISTS (
    SELECT 1 FROM channel_participants
    WHERE channel_id = $1 AND user_id = $2
      AND (banned_until IS NULL OR banned_until <= now())
);

-- name: DialogUnreadMarkCurrent :one
SELECT unread, changed_at
FROM user_dialog_unread_marks
WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3;

-- name: UpsertDialogUnreadMark :exec
INSERT INTO user_dialog_unread_marks (owner_id, peer_type, peer_id, unread, changed_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (owner_id, peer_type, peer_id) DO UPDATE
SET unread = EXCLUDED.unread,
    changed_at = EXCLUDED.changed_at;

-- name: NotifyDialogUnreadMark :exec
SELECT pg_notify('tg_updates', $1);

-- DialogUnreadMarksForPeers reads only currently visible, marked dialogs.
-- Removed or banned group/channel members cannot hydrate stale private marks.
-- name: DialogUnreadMarksForPeers :many
WITH selected AS (
    SELECT peer_types.peer_type, peer_ids.peer_id
    FROM unnest(sqlc.arg(peer_types)::smallint[]) WITH ORDINALITY AS peer_types(peer_type, ord)
    JOIN unnest(sqlc.arg(peer_ids)::bigint[]) WITH ORDINALITY AS peer_ids(peer_id, ord) USING (ord)
)
SELECT mark.peer_type, mark.peer_id
FROM user_dialog_unread_marks AS mark
JOIN selected
  ON selected.peer_type = mark.peer_type AND selected.peer_id = mark.peer_id
WHERE mark.owner_id = sqlc.arg(owner_id)
  AND mark.unread = true
  AND (
      (mark.peer_type = 1 AND EXISTS (
          SELECT 1 FROM dialogs AS dialog
          WHERE dialog.owner_id = mark.owner_id
            AND dialog.peer_type = mark.peer_type
            AND dialog.peer_id = mark.peer_id
      ))
      OR (mark.peer_type = 2 AND EXISTS (
          SELECT 1 FROM chat_participants AS participant
          WHERE participant.chat_id = mark.peer_id AND participant.user_id = mark.owner_id
      ))
      OR (mark.peer_type = 3 AND EXISTS (
          SELECT 1 FROM channel_participants AS participant
          WHERE participant.channel_id = mark.peer_id
            AND participant.user_id = mark.owner_id
            AND (participant.banned_until IS NULL OR participant.banned_until <= now())
      ))
  )
ORDER BY mark.peer_type, mark.peer_id;

-- DialogUnreadMarkStateForPeer rechecks visibility in the same statement as
-- the current value read so stale NOTIFY payloads cannot reveal a removed peer.
-- name: DialogUnreadMarkStateForPeer :one
SELECT mark.peer_type, mark.peer_id, mark.unread, mark.changed_at
FROM user_dialog_unread_marks AS mark
WHERE mark.owner_id = sqlc.arg(owner_id)
  AND mark.peer_type = sqlc.arg(peer_type)
  AND mark.peer_id = sqlc.arg(peer_id)
  AND (
      (mark.peer_type = 1 AND EXISTS (
          SELECT 1 FROM dialogs AS dialog
          WHERE dialog.owner_id = mark.owner_id
            AND dialog.peer_type = mark.peer_type
            AND dialog.peer_id = mark.peer_id
      ))
      OR (mark.peer_type = 2 AND EXISTS (
          SELECT 1 FROM chat_participants AS participant
          WHERE participant.chat_id = mark.peer_id AND participant.user_id = mark.owner_id
      ))
      OR (mark.peer_type = 3 AND EXISTS (
          SELECT 1 FROM channel_participants AS participant
          WHERE participant.channel_id = mark.peer_id
            AND participant.user_id = mark.owner_id
            AND (participant.banned_until IS NULL OR participant.banned_until <= now())
      ))
  );

-- DialogUnreadMarkChangesForOwnerSince is the separate guarded non-PTS recovery
-- stream. The extra row detects truncation; current visibility is rechecked and
-- false rows are retained to recover unmark transitions.
-- name: DialogUnreadMarkChangesForOwnerSince :many
SELECT mark.peer_type, mark.peer_id, mark.unread, mark.changed_at
FROM user_dialog_unread_marks AS mark
WHERE mark.owner_id = sqlc.arg(owner_id)
  AND mark.changed_at >= sqlc.arg(changed_since)
  AND (
      (mark.peer_type = 1 AND EXISTS (
          SELECT 1 FROM dialogs AS dialog
          WHERE dialog.owner_id = mark.owner_id
            AND dialog.peer_type = mark.peer_type
            AND dialog.peer_id = mark.peer_id
      ))
      OR (mark.peer_type = 2 AND EXISTS (
          SELECT 1 FROM chat_participants AS participant
          WHERE participant.chat_id = mark.peer_id AND participant.user_id = mark.owner_id
      ))
      OR (mark.peer_type = 3 AND EXISTS (
          SELECT 1 FROM channel_participants AS participant
          WHERE participant.channel_id = mark.peer_id
            AND participant.user_id = mark.owner_id
            AND (participant.banned_until IS NULL OR participant.banned_until <= now())
      ))
  )
ORDER BY mark.changed_at, mark.peer_type, mark.peer_id
LIMIT 501;
