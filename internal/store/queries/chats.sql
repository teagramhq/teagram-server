-- name: InsertChat :one
INSERT INTO chats (title, creator_id) VALUES ($1, $2)
RETURNING *;

-- name: InsertChatParticipant :exec
INSERT INTO chat_participants (chat_id, user_id, inviter_id) VALUES ($1, $2, $3);

-- name: ChatByID :one
SELECT * FROM chats WHERE id = $1;

-- ChatByIDForUpdate takes the chats row lock that serialises everything touching
-- one chat's member set: the fan-out reads the member set under it, and the
-- membership mutations take it before changing that set. See the lock-order
-- comment at the top of chats.go.
-- name: ChatByIDForUpdate :one
SELECT * FROM chats WHERE id = $1 FOR UPDATE;

-- ChatParticipants is ascending by user_id: the fan-out takes its advisory locks
-- in that order, so a stable ascending member list is what keeps it deadlock-free.
-- name: ChatParticipants :many
SELECT * FROM chat_participants WHERE chat_id = $1 ORDER BY user_id;

-- InsertChatParticipantIfAbsent reports 0 rows when the user is already a member,
-- which is what makes a repeated add a no-op instead of an error.
-- name: InsertChatParticipantIfAbsent :execrows
INSERT INTO chat_participants (chat_id, user_id, inviter_id) VALUES ($1, $2, $3)
ON CONFLICT (chat_id, user_id) DO NOTHING;

-- DeleteChatParticipant is called from exactly one place: removeParticipant in
-- chats.go, which also takes the removed user's advisory lock. See its comment.
-- name: DeleteChatParticipant :execrows
DELETE FROM chat_participants WHERE chat_id = $1 AND user_id = $2;

-- name: SetChatParticipantAdmin :execrows
UPDATE chat_participants SET is_admin = $3 WHERE chat_id = $1 AND user_id = $2;

-- name: BumpChatVersion :one
UPDATE chats SET version = version + 1 WHERE id = $1 RETURNING *;

-- name: InsertChatAdminEvent :one
INSERT INTO chat_admin_events (chat_id, user_id, is_admin, version)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- UpsertChatAdminEventRecipients keeps one current live recipient per
-- member/chat/target tuple for transient fanout in the role mutation transaction.
-- name: UpsertChatAdminEventRecipients :exec
INSERT INTO chat_admin_event_recipients (owner_id, chat_id, target_id, event_id)
SELECT recipient.owner_id, sqlc.arg(chat_id)::bigint, sqlc.arg(target_id)::bigint, sqlc.arg(event_id)::bigint
FROM unnest(sqlc.arg(owner_ids)::bigint[]) AS recipient(owner_id)
ON CONFLICT (owner_id, chat_id, target_id)
DO UPDATE SET event_id = EXCLUDED.event_id;

-- UpsertChatAdminStateMarkers keeps the latest pending pull version separately
-- so acknowledging one connection's difference does not suppress live fanout.
-- name: UpsertChatAdminStateMarkers :exec
INSERT INTO chat_admin_state_markers (owner_id, chat_id, target_id, event_id)
SELECT recipient.owner_id, sqlc.arg(chat_id)::bigint, sqlc.arg(target_id)::bigint, sqlc.arg(event_id)::bigint
FROM unnest(sqlc.arg(owner_ids)::bigint[]) AS recipient(owner_id)
ON CONFLICT (owner_id, chat_id, target_id)
DO UPDATE SET event_id = EXCLUDED.event_id;

-- ChatAdminEventRecipientsByEvent returns only recipients who remain members;
-- a delayed live push must not disclose an admin event after a member leaves.
-- name: ChatAdminEventRecipientsByEvent :many
SELECT recipient.owner_id
FROM chat_admin_event_recipients AS recipient
JOIN chat_admin_events AS event ON event.id = recipient.event_id
JOIN chat_participants AS participant
  ON participant.chat_id = recipient.chat_id
 AND participant.user_id = recipient.owner_id
WHERE recipient.event_id = $1
  AND recipient.chat_id = event.chat_id
  AND recipient.target_id = event.user_id
ORDER BY recipient.owner_id;

-- ChatAdminSnapshotsForMember returns the current role state for each target
-- with a pending pull marker. EventID lets the response-success hook consume
-- only versions included in this response, preserving newer concurrent changes.
-- name: ChatAdminSnapshotsForMember :many
SELECT marker.event_id,
       marker.chat_id,
       marker.target_id AS user_id,
       target.is_admin,
       chat.version
FROM chat_admin_state_markers AS marker
JOIN chat_admin_events AS event
  ON event.id = marker.event_id
 AND event.chat_id = marker.chat_id
 AND event.user_id = marker.target_id
JOIN chat_participants AS viewer
  ON viewer.chat_id = marker.chat_id
 AND viewer.user_id = marker.owner_id
JOIN chats AS chat ON chat.id = marker.chat_id
JOIN chat_participants AS target
  ON target.chat_id = marker.chat_id
 AND target.user_id = marker.target_id
WHERE marker.owner_id = $1
ORDER BY marker.event_id
LIMIT sqlc.arg(lim)::int;

-- DeleteChatAdminStateMarkersByEventIDs consumes the exact versions included
-- in a successfully written difference response.
-- name: DeleteChatAdminStateMarkersByEventIDs :exec
DELETE FROM chat_admin_state_markers
WHERE owner_id = sqlc.arg(owner_id)
  AND event_id = ANY(sqlc.arg(event_ids)::bigint[]);

-- name: ChatAdminEventsByIDs :many
SELECT * FROM chat_admin_events
WHERE id = ANY(sqlc.arg(event_ids)::bigint[])
ORDER BY id;

-- InsertChatRemovalEvent keeps the event independently of its pull marker so
-- a delayed pub/sub notification can still reach every live session after one
-- connection consumes the marker through getDifference.
-- name: InsertChatRemovalEvent :one
INSERT INTO chat_removal_events (owner_id, chat_id, version)
VALUES ($1, $2, $3)
RETURNING owner_id, chat_id, version;

-- ChatRemovalEventForOwnerChat reloads the immutable event named by a live
-- notification. Owner and chat form its unique identity because a chat is
-- deleted at most once and each current member receives one removal.
-- name: ChatRemovalEventForOwnerChat :one
SELECT * FROM chat_removal_events
WHERE owner_id = $1 AND chat_id = $2;

-- ChatRemovalSnapshotsForAuthKey returns removals not yet acknowledged by this
-- client's later updates.getDifference request. A prior successful write alone
-- does not suppress replay, because the client may not have applied the reply.
-- name: ChatRemovalSnapshotsForAuthKey :many
SELECT event.owner_id, event.chat_id, event.version
FROM chat_removal_events AS event
LEFT JOIN chat_removal_client_state AS client_state
  ON client_state.owner_id = event.owner_id
 AND client_state.auth_key_id = sqlc.arg(auth_key_id)
 AND client_state.chat_id = event.chat_id
WHERE event.owner_id = sqlc.arg(owner_id)
  AND client_state.acknowledged_at IS NULL
ORDER BY event.chat_id
LIMIT sqlc.arg(lim)::int;

-- MarkChatRemovalEventsDeliveredForAuthKey records a live push delivery. A live
-- push has no applied-state cursor, so it remains pending until a difference
-- response carries it and the client presents that response's state.
-- name: MarkChatRemovalEventsDeliveredForAuthKey :exec
INSERT INTO chat_removal_client_state (
    owner_id, auth_key_id, chat_id, delivered_at
)
SELECT sqlc.arg(owner_id)::bigint,
       sqlc.arg(auth_key_id)::bigint,
       pending.chat_id,
       clock_timestamp()
FROM unnest(sqlc.arg(chat_ids)::bigint[]) AS pending(chat_id)
ON CONFLICT (owner_id, auth_key_id, chat_id)
DO UPDATE SET delivered_at = EXCLUDED.delivered_at
WHERE chat_removal_client_state.acknowledged_at IS NULL;

-- MarkChatRemovalEventsIssuedForAuthKey records the difference state that
-- included these rows. The state is persisted after a successful reply write;
-- presenting its Date on a later getDifference is the client's acknowledgement.
-- name: MarkChatRemovalEventsIssuedForAuthKey :exec
INSERT INTO chat_removal_client_state (
    owner_id, auth_key_id, chat_id, delivered_at,
    difference_state_pts, difference_state_qts, difference_state_date, resume_date
)
SELECT sqlc.arg(owner_id)::bigint,
       sqlc.arg(auth_key_id)::bigint,
       pending.chat_id,
       clock_timestamp(),
       sqlc.arg(difference_state_pts)::bigint,
       sqlc.arg(difference_state_qts)::bigint,
       sqlc.arg(difference_state_date)::bigint,
       sqlc.arg(resume_date)::bigint
FROM unnest(sqlc.arg(chat_ids)::bigint[]) AS pending(chat_id)
ON CONFLICT (owner_id, auth_key_id, chat_id)
DO UPDATE SET delivered_at = EXCLUDED.delivered_at,
              difference_state_pts = EXCLUDED.difference_state_pts,
              difference_state_qts = EXCLUDED.difference_state_qts,
              difference_state_date = EXCLUDED.difference_state_date,
              resume_date = EXCLUDED.resume_date
WHERE chat_removal_client_state.acknowledged_at IS NULL;

-- ChatRemovalLatestDifferenceStateDateForAuthKey keeps newly issued page
-- tokens ahead of earlier tokens, including after those pages are acknowledged.
-- name: ChatRemovalLatestDifferenceStateDateForAuthKey :one
SELECT COALESCE(MAX(difference_state_date), 0)::bigint
FROM chat_removal_client_state
WHERE owner_id = sqlc.arg(owner_id)
  AND auth_key_id = sqlc.arg(auth_key_id);

-- A client confirms only rows issued with the Date it presents to
-- updates.getDifference, once its PTS and QTS have reached the returned state.
-- Return the logical Date used before the token so other Date-based streams resume.
-- name: AcknowledgeChatRemovalEventsForState :many
UPDATE chat_removal_client_state
SET acknowledged_at = COALESCE(acknowledged_at, clock_timestamp())
WHERE owner_id = sqlc.arg(owner_id)
  AND auth_key_id = sqlc.arg(auth_key_id)
  AND difference_state_date = sqlc.arg(difference_state_date)
  AND difference_state_pts <= sqlc.arg(request_pts)
  AND difference_state_qts <= sqlc.arg(request_qts)
RETURNING resume_date;

-- name: SetChatTitle :one
UPDATE chats SET title = $2, version = version + 1 WHERE id = $1 RETURNING *;

-- ChatAboutByID returns an empty description when no chat_about row exists.
-- Older groups stay empty without a backfill, and clearing a description deletes
-- its row.
-- name: ChatAboutByID :one
SELECT COALESCE((SELECT about FROM chat_about WHERE chat_id = sqlc.arg(chat_id)::bigint), '')::text AS about;

-- name: SetChatAbout :exec
INSERT INTO chat_about (chat_id, about) VALUES ($1, $2)
ON CONFLICT (chat_id) DO UPDATE SET about = EXCLUDED.about;

-- name: DeleteChatAbout :execrows
DELETE FROM chat_about WHERE chat_id = $1;

-- ChatAboutsByIDsForMember hydrates descriptions only for the selected member
-- chats in the same read snapshot as the rest of full-chat info.
-- name: ChatAboutsByIDsForMember :many
SELECT participant.chat_id, COALESCE(description.about, '')::text AS about
FROM chat_participants AS participant
LEFT JOIN chat_about AS description ON description.chat_id = participant.chat_id
WHERE participant.user_id = sqlc.arg(user_id)::bigint
  AND participant.chat_id = ANY(sqlc.arg(chat_ids)::bigint[])
ORDER BY participant.chat_id;

-- name: SetChatDefaultBannedRights :one
UPDATE chats
SET default_banned_rights = $2, version = version + 1
WHERE id = $1
RETURNING *;

-- name: IsChatMember :one
SELECT EXISTS(SELECT 1 FROM chat_participants WHERE chat_id = $1 AND user_id = $2);

-- name: ChatsForUser :many
SELECT c.* FROM chats c
JOIN chat_participants p ON p.chat_id = c.id
WHERE p.user_id = $1
ORDER BY c.id;

-- A missing chat and a chat without a membership row for the viewer both
-- produce no result. Keep the read on the basic-chat tables so equal numeric
-- ids in users or channels cannot resolve to those peer types.
-- name: ChatsByIDsForMember :many
SELECT c.* FROM chats c
JOIN chat_participants p ON p.chat_id = c.id
WHERE p.user_id = sqlc.arg(user_id)::bigint
  AND c.id = ANY(sqlc.arg(chat_ids)::bigint[])
ORDER BY c.id;

-- name: ChatParticipantCountsByChatIDs :many
SELECT chat_id, count(*)::bigint AS participant_count
FROM chat_participants
WHERE chat_id = ANY(sqlc.arg(chat_ids)::bigint[])
GROUP BY chat_id
ORDER BY chat_id;

-- SetChatPinnedMessage sets or clears the pinned message id on a chat.
-- The pinned_message_id stores the creator-owned copy's local_id; fanout_id
-- resolves that logical message to each member's local copy. NULL clears the pin.
-- name: SetChatPinnedMessage :one
UPDATE chats SET pinned_message_id = $2, version = version + 1 WHERE id = $1 RETURNING *;

-- GetChatPinnedMessage reads the current pinned message id for a chat.
-- name: GetChatPinnedMessage :one
SELECT pinned_message_id FROM chats WHERE id = $1;

-- ChatPinnedMessageForOwner resolves the creator-owned pin to the requested
-- member's copy. fanout_id identifies one logical chat message across each
-- member-owned row; the returned local_id always belongs to owner_id.
-- Deleted or missing source/member copies intentionally produce no row.
-- name: ChatPinnedMessageForOwner :one
SELECT viewer_copy.local_id
FROM chats c
JOIN chat_participants p ON p.chat_id = c.id AND p.user_id = sqlc.arg(owner_id)::bigint
JOIN messages creator_copy
  ON creator_copy.owner_id = c.creator_id
 AND creator_copy.local_id = c.pinned_message_id
 AND creator_copy.peer_type = sqlc.arg(peer_type)::smallint
 AND creator_copy.peer_id = c.id
 AND creator_copy.fanout_id <> 0
 AND creator_copy.deleted = false
JOIN messages viewer_copy
  ON viewer_copy.owner_id = p.user_id
 AND viewer_copy.fanout_id = creator_copy.fanout_id
 AND viewer_copy.peer_type = sqlc.arg(peer_type)::smallint
 AND viewer_copy.peer_id = c.id
 AND viewer_copy.deleted = false
WHERE c.id = sqlc.arg(chat_id)::bigint
  AND c.pinned_message_id IS NOT NULL;

-- SearchPinnedChatMessageForOwner returns the active viewer-owned copy of the
-- chat's current pin. The creator row identifies the logical fan-out, while
-- the participant join and viewer copy keep the result scoped to this member's
-- local id space. Search text and pagination narrow that single pinned row.
-- name: SearchPinnedChatMessageForOwner :many
SELECT viewer_copy.*
FROM chats c
JOIN chat_participants p
  ON p.chat_id = c.id AND p.user_id = sqlc.arg(owner_id)::bigint
JOIN messages creator_copy
  ON creator_copy.owner_id = c.creator_id
 AND creator_copy.local_id = c.pinned_message_id
 AND creator_copy.peer_type = sqlc.arg(peer_type)::smallint
 AND creator_copy.peer_id = c.id
 AND creator_copy.fanout_id <> 0
 AND creator_copy.deleted = false
JOIN messages viewer_copy
  ON viewer_copy.owner_id = p.user_id
 AND viewer_copy.fanout_id = creator_copy.fanout_id
 AND viewer_copy.peer_type = sqlc.arg(peer_type)::smallint
 AND viewer_copy.peer_id = c.id
 AND viewer_copy.deleted = false
WHERE c.id = sqlc.arg(chat_id)::bigint
  AND c.pinned_message_id IS NOT NULL
  AND (sqlc.arg(query)::text = '' OR viewer_copy.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query)))
  AND (sqlc.arg(offset_id)::bigint = 0 OR viewer_copy.local_id < sqlc.arg(offset_id)::bigint)
ORDER BY viewer_copy.local_id DESC
LIMIT sqlc.arg(lim)::int;

-- ChatPinSnapshot reads the selected pin and each member's local copy from one
-- statement snapshot, so a concurrent repin cannot split one notification.
-- Missing or deleted copies remain NULL while the participant still receives
-- the pinned state.
-- name: ChatPinSnapshot :many
SELECT p.user_id,
       c.pinned_message_id,
       viewer_copy.local_id
FROM chats c
JOIN chat_participants p ON p.chat_id = c.id
LEFT JOIN messages creator_copy
  ON creator_copy.owner_id = c.creator_id
 AND creator_copy.local_id = c.pinned_message_id
 AND creator_copy.peer_type = sqlc.arg(peer_type)::smallint
 AND creator_copy.peer_id = c.id
 AND creator_copy.fanout_id <> 0
 AND creator_copy.deleted = false
LEFT JOIN messages viewer_copy
  ON viewer_copy.owner_id = p.user_id
 AND viewer_copy.fanout_id = creator_copy.fanout_id
 AND viewer_copy.peer_type = sqlc.arg(peer_type)::smallint
 AND viewer_copy.peer_id = c.id
 AND viewer_copy.deleted = false
WHERE c.id = sqlc.arg(chat_id)::bigint
ORDER BY p.user_id;

-- ChatPinEventCopies lists, for one creator-owned chat message, every current
-- participant's copy of it, deleted copies included: a pin or unpin records
-- one event per row, and a deleted copy still names the chat the pin state
-- belongs to. A member with no copy at all records nothing.
-- name: ChatPinEventCopies :many
SELECT viewer_copy.owner_id, viewer_copy.local_id
FROM messages creator_copy
JOIN messages viewer_copy
  ON viewer_copy.fanout_id = creator_copy.fanout_id
 AND viewer_copy.peer_type = creator_copy.peer_type
 AND viewer_copy.peer_id = creator_copy.peer_id
JOIN chat_participants p
  ON p.chat_id = creator_copy.peer_id
 AND p.user_id = viewer_copy.owner_id
WHERE creator_copy.owner_id = sqlc.arg(creator_id)::bigint
  AND creator_copy.local_id = sqlc.arg(local_id)::bigint
  AND creator_copy.peer_type = sqlc.arg(peer_type)::smallint
  AND creator_copy.peer_id = sqlc.arg(chat_id)::bigint
  AND creator_copy.fanout_id <> 0
ORDER BY viewer_copy.owner_id;

-- ChatReadParticipantsForMessage authorizes the sender's live outgoing copy,
-- applies Telegram's group-size and message-age limits, and selects only
-- current-member receipts for that chat and logical message. Authorization,
-- eligibility, and receipt selection share one statement snapshot.
-- name: ChatReadParticipantsForMessage :many
WITH source AS (
    SELECT message.fanout_id, message.date
    FROM messages AS message
    JOIN chat_participants AS sender
      ON sender.chat_id = message.peer_id
     AND sender.user_id = message.owner_id
    WHERE message.owner_id = sqlc.arg(owner_id)::bigint
      AND message.local_id = sqlc.arg(local_id)::bigint
      AND message.peer_type = sqlc.arg(peer_type)::smallint
      AND message.peer_id = sqlc.arg(peer_id)::bigint
      AND message.out = true
      AND message.deleted = false
      AND message.fanout_id <> 0
    LIMIT 1
), eligible_source AS (
    SELECT source.fanout_id
    FROM source
    WHERE source.date > statement_timestamp()
            - make_interval(secs => sqlc.arg(expire_period)::double precision)
      AND (SELECT count(*) FROM chat_participants
           WHERE chat_id = sqlc.arg(peer_id)::bigint)
            < sqlc.arg(size_threshold)::bigint
)
SELECT EXISTS (SELECT 1 FROM source) AS authorized,
       EXISTS (SELECT 1 FROM eligible_source) AS eligible,
       receipt.reader_id,
       receipt.read_at
FROM (VALUES (true)) AS singleton(available)
LEFT JOIN eligible_source AS eligible ON true
LEFT JOIN chat_read_receipts AS receipt
  ON receipt.chat_id = sqlc.arg(peer_id)::bigint
 AND receipt.fanout_id = eligible.fanout_id
 AND receipt.reader_id <> sqlc.arg(owner_id)::bigint
 AND EXISTS (
     SELECT 1
     FROM chat_participants AS current_member
     WHERE current_member.chat_id = receipt.chat_id
       AND current_member.user_id = receipt.reader_id
 )
ORDER BY receipt.reader_id;
