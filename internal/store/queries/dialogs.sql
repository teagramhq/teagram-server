-- UpsertDialog advances top_message and adds an unread delta (0 for the sender's
-- outbox side, 1 for the recipient's inbox side).
-- name: UpsertDialog :exec
INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message, unread_count)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (owner_id, peer_type, peer_id) DO UPDATE
  SET top_message  = EXCLUDED.top_message,
      unread_count = dialogs.unread_count + EXCLUDED.unread_count;

-- name: DialogsForOwner :many
SELECT * FROM dialogs
WHERE owner_id = sqlc.arg(owner_id)
  AND (
        (sqlc.arg(offset_id)::bigint = 0 AND sqlc.arg(offset_peer_type)::smallint = 0)
        OR top_message < sqlc.arg(offset_id)::bigint
        OR (top_message = sqlc.arg(offset_id)::bigint
            AND sqlc.arg(offset_peer_type)::smallint <> 0
            AND (peer_type, peer_id) > (sqlc.arg(offset_peer_type)::smallint, sqlc.arg(offset_peer_id)::bigint))
  )
ORDER BY top_message DESC, peer_type, peer_id
LIMIT sqlc.arg(lim)::int;

-- CountDialogsForOwner is the exact total the truncated reply advertises. It is
-- maintained for every dialogs row, unfiltered by offset, so this lookup stays
-- constant cost as the owner's list grows.
-- name: CountDialogsForOwner :one
SELECT COALESCE((
    SELECT dialog_count::int
    FROM owner_dialog_counts
    WHERE owner_id = $1 AND initialized
), 0)::int;

-- PeerDialogExists is the caller-owned 1:1 dialog predicate for peer settings.
-- name: PeerDialogExists :one
SELECT EXISTS (
    SELECT 1 FROM dialogs
    WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
);

-- DeleteDialogForPeer removes only the caller's dialog row after a non-clear
-- history deletion.
-- name: DeleteDialogForPeer :exec
DELETE FROM dialogs
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND peer_type = sqlc.arg(peer_type)::smallint
  AND peer_id = sqlc.arg(peer_id)::bigint;

-- RefreshDialogSummaryAfterDelete repairs the live top message and unread count
-- after a message delete. Both values are derived from non-deleted rows owned
-- by this dialog's owner.
-- name: RefreshDialogSummaryAfterDelete :exec
UPDATE dialogs AS d
SET top_message = COALESCE((
        SELECT max(m.local_id)
        FROM messages AS m
        WHERE m.owner_id = d.owner_id AND m.peer_type = d.peer_type
          AND m.peer_id = d.peer_id AND m.deleted = false
    ), 0),
    unread_count = (
        SELECT count(*)::int
        FROM messages AS m
        WHERE m.owner_id = d.owner_id AND m.peer_type = d.peer_type
          AND m.peer_id = d.peer_id AND m.deleted = false AND m.out = false
          AND m.local_id > d.read_inbox_max_id
    )
WHERE d.owner_id = sqlc.arg(owner_id)::bigint
  AND d.peer_type = sqlc.arg(peer_type)::smallint
  AND d.peer_id = sqlc.arg(peer_id)::bigint;

-- DialogsFirstPageWithPins returns the newest dialog page. It marks only
-- currently visible pins. Removed group members keep their dialog row, but
-- their stale pin neither hides the row nor remains client-visible. The first
-- page and cursor pages are separate statements so a cached generic plan never
-- has to keep a parameter-dependent OR that defeats the page index.
-- name: DialogsFirstPageWithPins :many
SELECT d.owner_id, d.peer_id, d.top_message, d.unread_count,
       d.read_inbox_max_id, d.read_outbox_max_id, d.peer_type,
       (EXISTS (
            SELECT 1 FROM user_dialog_pins p
            WHERE p.owner_id = d.owner_id AND p.peer_type = d.peer_type
              AND p.peer_id = d.peer_id AND p.position IS NOT NULL
        ) AND (d.peer_type <> 2 OR EXISTS (
            SELECT 1 FROM chat_participants cp
            WHERE cp.chat_id = d.peer_id AND cp.user_id = d.owner_id
        ))) AS pinned
FROM dialogs d
WHERE d.owner_id = sqlc.arg(owner_id)
  AND (NOT sqlc.arg(exclude_pinned)::boolean OR NOT (
        EXISTS (
            SELECT 1 FROM user_dialog_pins p
            WHERE p.owner_id = d.owner_id AND p.peer_type = d.peer_type
              AND p.peer_id = d.peer_id AND p.position IS NOT NULL
        ) AND (d.peer_type <> 2 OR EXISTS (
            SELECT 1 FROM chat_participants cp
            WHERE cp.chat_id = d.peer_id AND cp.user_id = d.owner_id
        ))
  ))
ORDER BY d.top_message DESC, d.peer_type, d.peer_id
LIMIT sqlc.arg(lim)::int;

-- DialogsCursorPageWithPins continues DialogsFirstPageWithPins with a stable
-- (top_message, peer_type, peer_id) cursor so equal top messages, including
-- empty dialogs at zero, can be paged. The top_message bound keeps the cursor
-- an index range in generic plans.
-- name: DialogsCursorPageWithPins :many
SELECT d.owner_id, d.peer_id, d.top_message, d.unread_count,
       d.read_inbox_max_id, d.read_outbox_max_id, d.peer_type,
       (EXISTS (
            SELECT 1 FROM user_dialog_pins p
            WHERE p.owner_id = d.owner_id AND p.peer_type = d.peer_type
              AND p.peer_id = d.peer_id AND p.position IS NOT NULL
        ) AND (d.peer_type <> 2 OR EXISTS (
            SELECT 1 FROM chat_participants cp
            WHERE cp.chat_id = d.peer_id AND cp.user_id = d.owner_id
        ))) AS pinned
FROM dialogs d
WHERE d.owner_id = sqlc.arg(owner_id)
  AND d.top_message <= sqlc.arg(offset_id)::bigint
  AND (
        d.top_message < sqlc.arg(offset_id)::bigint
        OR (sqlc.arg(offset_peer_type)::smallint <> 0
            AND (d.peer_type, d.peer_id) > (sqlc.arg(offset_peer_type)::smallint, sqlc.arg(offset_peer_id)::bigint))
  )
  AND (NOT sqlc.arg(exclude_pinned)::boolean OR NOT (
        EXISTS (
            SELECT 1 FROM user_dialog_pins p
            WHERE p.owner_id = d.owner_id AND p.peer_type = d.peer_type
              AND p.peer_id = d.peer_id AND p.position IS NOT NULL
        ) AND (d.peer_type <> 2 OR EXISTS (
            SELECT 1 FROM chat_participants cp
            WHERE cp.chat_id = d.peer_id AND cp.user_id = d.owner_id
        ))
  ))
ORDER BY d.top_message DESC, d.peer_type, d.peer_id
LIMIT sqlc.arg(lim)::int;

-- CountDialogsExcludingPinned is the unpaged count for the same active-pin
-- predicate as DialogsForOwnerWithPins.
-- name: CountDialogsExcludingPinned :one
SELECT count(*)::int
FROM dialogs d
WHERE d.owner_id = $1
  AND NOT (
        EXISTS (
            SELECT 1 FROM user_dialog_pins p
            WHERE p.owner_id = d.owner_id AND p.peer_type = d.peer_type
              AND p.peer_id = d.peer_id AND p.position IS NOT NULL
        ) AND (d.peer_type <> 2 OR EXISTS (
            SELECT 1 FROM chat_participants cp
            WHERE cp.chat_id = d.peer_id AND cp.user_id = d.owner_id
        ))
  );

-- AdvanceReadInbox raises the reader's read_inbox_max_id monotonically and
-- recomputes unread as the count of still-unread inbound messages above it
-- only when the marker advances. A stale/equal read is a no-op for unread
-- state and should not scan the conversation.
-- name: AdvanceReadInbox :one
UPDATE dialogs SET
  read_inbox_max_id = GREATEST(read_inbox_max_id, sqlc.arg(max_id)::bigint),
  read_outbox_max_id = CASE
    WHEN dialogs.owner_id = dialogs.peer_id
      THEN GREATEST(dialogs.read_outbox_max_id, sqlc.arg(max_id)::bigint)
    ELSE dialogs.read_outbox_max_id
  END,
  unread_count = CASE
    WHEN sqlc.arg(max_id)::bigint <= dialogs.read_inbox_max_id THEN dialogs.unread_count
    ELSE (
      SELECT count(*) FROM messages m
      WHERE m.owner_id = dialogs.owner_id AND m.peer_type = dialogs.peer_type AND m.peer_id = dialogs.peer_id
        AND m.out = false AND m.deleted = false
        AND m.local_id > sqlc.arg(max_id)::bigint
    )::int
  END
WHERE owner_id = sqlc.arg(owner_id)::bigint AND peer_type = sqlc.arg(peer_type)::smallint
  AND peer_id = sqlc.arg(peer_id)::bigint
RETURNING read_inbox_max_id, unread_count;

-- AdvanceChatReadInbox leaves the chat's outbox marker alone even when its id
-- numerically equals the reader's user id.
-- name: AdvanceChatReadInbox :one
UPDATE dialogs SET
  read_inbox_max_id = GREATEST(read_inbox_max_id, sqlc.arg(max_id)::bigint),
  unread_count = (
    SELECT count(*) FROM messages m
    WHERE m.owner_id = dialogs.owner_id AND m.peer_type = dialogs.peer_type AND m.peer_id = dialogs.peer_id
      AND m.out = false AND m.deleted = false
      AND m.local_id > GREATEST(dialogs.read_inbox_max_id, sqlc.arg(max_id)::bigint)
  )::int
WHERE owner_id = sqlc.arg(owner_id)::bigint AND peer_type = sqlc.arg(peer_type)::smallint
  AND peer_id = sqlc.arg(peer_id)::bigint
RETURNING read_inbox_max_id, unread_count;

-- AdvanceReadOutbox raises the peer's read_outbox_max_id monotonically.
-- name: AdvanceReadOutbox :execrows
UPDATE dialogs SET read_outbox_max_id = GREATEST(read_outbox_max_id, $4)
WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3;

-- ReadMarkers reads the current inbox and outbox boundaries for one dialog.
-- name: ReadMarkers :one
SELECT read_inbox_max_id, read_outbox_max_id FROM dialogs
WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3;

-- name: MaxChatReadMessageID :one
SELECT COALESCE(MAX(local_id), 0)::bigint AS max_id
FROM messages
WHERE owner_id = $1 AND peer_type = $2 AND peer_id = $3
  AND deleted = false AND local_id <= $4;

-- ChatReadReceiptTargets maps covered inbound chat copies to each current
-- sender's own outbound message-id space. The current participant list is
-- supplied only after its owners have been locked and rechecked.
-- name: ChatReadReceiptTargets :many
WITH covered AS (
    SELECT DISTINCT m.from_id, m.fanout_id
    FROM messages AS m
    WHERE m.owner_id = sqlc.arg(owner_id)::bigint
      AND m.peer_type = sqlc.arg(peer_type)::smallint
      AND m.peer_id = sqlc.arg(peer_id)::bigint
      AND m.out = false AND m.deleted = false AND m.fanout_id <> 0
      AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
      AND m.local_id > sqlc.arg(after_id)::bigint
      AND m.local_id <= sqlc.arg(max_id)::bigint
      AND m.from_id = ANY(sqlc.arg(member_ids)::bigint[])
)
SELECT covered.from_id AS sender_id, MAX(sender.local_id)::bigint AS max_id
FROM covered
JOIN messages AS sender
  ON sender.owner_id = covered.from_id
 AND sender.fanout_id = covered.fanout_id
 AND sender.fanout_id <> 0
 AND sender.out = true
 AND sender.peer_type = sqlc.arg(peer_type)::smallint
 AND sender.peer_id = sqlc.arg(peer_id)::bigint
GROUP BY covered.from_id
ORDER BY covered.from_id;

-- CaptureChatReadReceipts records one immutable first-read date for each live
-- inbound copy newly covered by this authorized read advance. The message date
-- is retained separately for the seven-day send-age window and cleanup.
-- name: CaptureChatReadReceipts :execrows
INSERT INTO chat_read_receipts (chat_id, fanout_id, reader_id, sent_at)
SELECT sqlc.arg(peer_id)::bigint, m.fanout_id, sqlc.arg(owner_id)::bigint, m.date
FROM messages AS m
WHERE m.owner_id = sqlc.arg(owner_id)::bigint
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
  AND m.out = false AND m.deleted = false AND m.fanout_id <> 0
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
  AND m.local_id > sqlc.arg(after_id)::bigint
  AND m.local_id <= sqlc.arg(max_id)::bigint
  AND m.from_id = ANY(sqlc.arg(member_ids)::bigint[])
  AND m.date > statement_timestamp() - interval '604800 seconds'
  AND EXISTS (
      SELECT 1 FROM messages AS sender
      WHERE sender.owner_id = m.from_id
        AND sender.fanout_id = m.fanout_id
        AND sender.out = true
        AND sender.peer_type = m.peer_type
        AND sender.peer_id = m.peer_id
  )
ON CONFLICT (chat_id, fanout_id, reader_id) DO NOTHING;

-- DeleteExpiredChatReadReceipts retires one bounded batch. The caller repeats
-- passes until the expired backlog is drained; every pass uses the database
-- clock and measures retention from the original message send date.
-- name: DeleteExpiredChatReadReceipts :execrows
WITH expired AS (
    SELECT chat_id, fanout_id, reader_id
    FROM chat_read_receipts
    WHERE sent_at <= statement_timestamp() - interval '604800 seconds'
    ORDER BY sent_at, chat_id, fanout_id, reader_id
    LIMIT sqlc.arg(lim)::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM chat_read_receipts AS receipt
USING expired
WHERE receipt.chat_id = expired.chat_id
  AND receipt.fanout_id = expired.fanout_id
  AND receipt.reader_id = expired.reader_id;
