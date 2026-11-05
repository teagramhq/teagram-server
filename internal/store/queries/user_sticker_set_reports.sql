-- Resolve the report target in the reporter's own message space. Chat copies
-- remain stored after a member leaves, so require current membership as well.
-- name: UserStickerSetsInOwnMessagesForReport :many
SELECT DISTINCT m.local_id, s.id AS set_id
FROM messages m
LEFT JOIN LATERAL (
    SELECT item.set_id
    FROM user_created_sticker_set_items item
    WHERE item.file_id = m.sticker_document_id
    UNION
    SELECT item.set_id
    FROM unnest(message_custom_emoji_document_ids(m.entities)) AS custom_emoji(document_id)
    JOIN user_created_sticker_set_items item
      ON item.file_id = CASE
          WHEN pg_input_is_valid(custom_emoji.document_id, 'bigint')
          THEN custom_emoji.document_id::bigint
      END
) i ON true
LEFT JOIN user_created_sticker_sets s ON s.id = i.set_id AND s.is_live = true
WHERE m.owner_id = sqlc.arg(reporter_id)::bigint
  AND m.local_id = ANY(sqlc.arg(message_ids)::bigint[])
  AND m.peer_type = sqlc.arg(peer_type)::smallint
  AND m.peer_id = sqlc.arg(peer_id)::bigint
  AND m.deleted = false
  AND (m.expires_at IS NULL OR m.expires_at > statement_timestamp())
  AND (
      m.peer_type <> 2
      OR EXISTS (
          SELECT 1 FROM chat_participants cp
          WHERE cp.chat_id = m.peer_id AND cp.user_id = m.owner_id
      )
  )
ORDER BY s.id;

-- Channel posts are visible only while the reporter is an unbanned member and
-- the post is above their history floor. The visibility check and set lookup
-- share one statement snapshot.
-- name: UserStickerSetsInReadableChannelMessagesForReport :many
SELECT DISTINCT post.local_id, s.id AS set_id
FROM channel_messages post
JOIN channel_participants participant
  ON participant.channel_id = post.channel_id
 AND participant.user_id = sqlc.arg(reporter_id)::bigint
JOIN channels channel ON channel.id = post.channel_id AND channel.deleted_at IS NULL
LEFT JOIN LATERAL (
    SELECT item.set_id
    FROM user_created_sticker_set_items item
    WHERE item.file_id = post.sticker_document_id
    UNION
    SELECT item.set_id
    FROM unnest(message_custom_emoji_document_ids(post.entities)) AS custom_emoji(document_id)
    JOIN user_created_sticker_set_items item
      ON item.file_id = CASE
          WHEN pg_input_is_valid(custom_emoji.document_id, 'bigint')
          THEN custom_emoji.document_id::bigint
      END
) i ON true
LEFT JOIN user_created_sticker_sets s ON s.id = i.set_id AND s.is_live = true
WHERE post.channel_id = sqlc.arg(peer_id)::bigint
  AND post.local_id = ANY(sqlc.arg(message_ids)::bigint[])
  AND post.deleted = false
  AND (post.expires_at IS NULL OR post.expires_at > statement_timestamp())
  AND post.local_id > participant.available_min_id
  AND (participant.banned_until IS NULL OR participant.banned_until <= statement_timestamp())
ORDER BY s.id;

-- Takedown locks the set row FOR UPDATE. Sharing the live rows here means a
-- report either commits before the takedown and is actioned there, or observes
-- the set as unavailable and is not inserted.
-- name: LockLiveUserStickerSetReportTargets :many
SELECT id FROM user_created_sticker_sets
WHERE id = ANY(sqlc.arg(set_ids)::bigint[]) AND is_live = true
ORDER BY id
FOR SHARE;

-- name: ExistingOpenUserStickerSetReportTargets :many
WITH requested AS (
    SELECT set_id, message_id
    FROM unnest(sqlc.arg(set_ids)::bigint[]) WITH ORDINALITY AS set_targets(set_id, position)
    JOIN unnest(sqlc.arg(message_ids)::bigint[]) WITH ORDINALITY AS message_targets(message_id, position)
      USING (position)
)
SELECT report.set_id, report.message_id
FROM user_sticker_set_reports report
JOIN requested ON requested.set_id = report.set_id AND requested.message_id = report.message_id
WHERE report.reporter_id = sqlc.arg(reporter_id)::bigint
  AND report.message_peer_type = sqlc.arg(message_peer_type)::smallint
  AND report.message_peer_id = sqlc.arg(message_peer_id)::bigint
  AND report.reason = sqlc.arg(reason)::text
  AND report.comment = sqlc.arg(comment)::text
  AND report.status = 'open';

-- Closed reports still count during the rolling hour.
-- name: CountRecentUserStickerSetReports :one
SELECT count(*) FROM user_sticker_set_reports
WHERE reporter_id = sqlc.arg(reporter_id)::bigint
  AND created_at > statement_timestamp() - interval '1 hour';

-- name: InsertUserStickerSetReport :exec
INSERT INTO user_sticker_set_reports (
    reporter_id, set_id, message_peer_type, message_peer_id, message_id, reason, comment
)
VALUES (
    sqlc.arg(reporter_id), sqlc.arg(set_id), sqlc.arg(message_peer_type), sqlc.arg(message_peer_id),
    sqlc.arg(message_id), sqlc.arg(reason), sqlc.arg(comment)
)
ON CONFLICT (reporter_id, set_id, message_peer_type, message_peer_id, message_id, reason, comment)
    WHERE status = 'open'
DO NOTHING;

-- Page report groups rather than individual rows so one set's open reports stay
-- together in the admin surface.
-- name: AdminOpenUserStickerSetReportSetIDsPage :many
SELECT set_id
FROM user_sticker_set_reports
WHERE status = 'open'
GROUP BY set_id
ORDER BY min(created_at), set_id
LIMIT sqlc.arg(limit_count)::int
OFFSET sqlc.arg(offset_count)::int;

-- name: AdminOpenUserStickerSetReportsForSets :many
SELECT r.id AS report_id, r.set_id, s.title AS set_title, s.short_name AS set_short_name,
       s.is_live AS set_is_live, r.reporter_id, r.message_peer_type, r.message_peer_id,
       r.message_id, r.reason, r.comment, r.created_at
FROM unnest(sqlc.arg(set_ids)::bigint[]) AS requested(set_id)
JOIN user_created_sticker_sets s ON s.id = requested.set_id
CROSS JOIN LATERAL (
    SELECT report.*
    FROM user_sticker_set_reports report
    WHERE report.status = 'open' AND report.set_id = requested.set_id
    ORDER BY report.created_at, report.id
    LIMIT sqlc.arg(limit_count)::int
    OFFSET CASE
        WHEN requested.set_id = sqlc.arg(page_set_id)::bigint THEN sqlc.arg(offset_count)::int
        ELSE 0
    END
) r
ORDER BY r.set_id, r.created_at, r.id;

-- name: DismissUserStickerSetReport :execrows
UPDATE user_sticker_set_reports
SET status = 'dismissed', closed_at = statement_timestamp()
WHERE id = sqlc.arg(report_id)::bigint AND status = 'open';

-- Taking a set down actions every open report for that set in the same
-- transaction as the takedown state change.
-- name: ActionOpenUserStickerSetReports :execrows
UPDATE user_sticker_set_reports
SET status = 'actioned', closed_at = statement_timestamp()
WHERE set_id = sqlc.arg(set_id)::bigint AND status = 'open';

-- name: DeleteExpiredUserStickerSetReports :execrows
WITH expired AS (
    SELECT id FROM user_sticker_set_reports
    WHERE status <> 'open'
      AND closed_at < statement_timestamp() - interval '180 days'
    ORDER BY closed_at, id
    LIMIT sqlc.arg(batch_size)::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM user_sticker_set_reports report
USING expired
WHERE report.id = expired.id;
