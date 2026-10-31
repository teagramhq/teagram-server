-- name: EnsureChannelState :exec
INSERT INTO channel_state (channel_id) VALUES ($1) ON CONFLICT (channel_id) DO NOTHING;

-- name: GetChannelState :one
SELECT * FROM channel_state WHERE channel_id = $1;

-- LockChannelState takes the channel_state row lock ahead of the dedup read, so
-- two concurrent posts carrying the same random_id cannot both miss it. It is
-- the same row the bump below locks, taken one statement earlier.
-- name: LockChannelState :one
SELECT * FROM channel_state WHERE channel_id = $1 FOR UPDATE;

-- BumpChannelState allocates the next local_id and advances pts in one step. It
-- returns the new pts and the local_id just consumed (pre-increment value).
-- Like update_state, the final legal allocation may leave next_local_id at
-- 2147483648; only the current ID and resulting pts must fit the wire width.
-- name: BumpChannelState :one
UPDATE channel_state
SET pts = pts + 1, next_local_id = next_local_id + 1, date = now()
WHERE channel_id = $1 AND pts < 2147483647 AND next_local_id <= 2147483647
RETURNING pts, (next_local_id - 1)::bigint AS local_id;

-- BumpChannelPtsOnly appends a durable edit/delete event without allocating a
-- new message id.
-- name: BumpChannelPtsOnly :one
UPDATE channel_state SET pts = pts + 1, date = now()
WHERE channel_id = $1 AND pts < 2147483647
RETURNING pts;

-- ChannelEventsWindow returns events in (from_pts, to_pts] ordered, capped by
-- lim. The upper bound pins the read to a pts snapshot so the difference never
-- advertises a pts past an event it omitted.
-- name: ChannelEventsWindow :many
SELECT * FROM channel_events
WHERE channel_id = sqlc.arg(channel_id) AND pts > sqlc.arg(from_pts) AND pts <= sqlc.arg(to_pts)
ORDER BY pts
LIMIT sqlc.arg(lim)::int;

-- NewChannelPostPts is NewMessagePts for a channel's log, and carries the same
-- contract: one row per post, type = 1 spelled as a literal for the partial
-- index.
-- name: NewChannelPostPts :one
SELECT pts FROM channel_events
WHERE channel_id = $1 AND local_id = $2 AND type = 1;

-- name: InsertChannelEvent :exec
INSERT INTO channel_events (channel_id, pts, type, local_id) VALUES ($1, $2, $3, $4);

-- name: InsertChannelMessage :exec
INSERT INTO channel_messages (
    channel_id, local_id, from_id, message, random_id, file_id,
    sticker_document_id, reply_to_msg_id, post_author, entities
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF(sqlc.arg(entities)::text, '')::jsonb);

-- name: SetChannelMessageEditDate :execrows
UPDATE channel_messages SET edit_date = now()
WHERE channel_id = $1 AND local_id = $2 AND deleted = false;

-- name: InsertChannelCreateMessage :exec
INSERT INTO channel_messages (channel_id, local_id, from_id, message, action_type)
VALUES ($1, $2, $3, $4, 1);

-- name: InsertChannelTitleChangeMessage :exec
INSERT INTO channel_messages (channel_id, local_id, from_id, message, action_type)
VALUES ($1, $2, $3, $4, 2);

-- Photo actions share the channel log. action_type 4 carries the selected
-- photo file identity; 5 is an empty clear and carries no file.
-- name: InsertChannelPhotoChangeMessage :exec
INSERT INTO channel_messages (channel_id, local_id, from_id, message, file_id, action_type)
VALUES ($1, $2, $3, '', $4, $5);

-- ChannelPostExistsActive returns the local_id of an existing, non-deleted post
-- in channelID. Used to validate reply_to_msg_id inside the post transaction.
--
-- LOCK INVARIANT: callers must hold the channel_state row lock (LockChannelState)
-- before calling this. A future channel-post delete path must acquire the same
-- lock before writing deleted = true, or the snapshot read here can race it and
-- accept a reply reference to a post that is deleted by the time we commit.
-- name: ChannelPostExistsActive :one
SELECT cm.local_id
FROM channel_messages cm
JOIN channel_participants cp ON cp.channel_id = cm.channel_id AND cp.user_id = $3
WHERE cm.channel_id = $1 AND cm.local_id = $2
  AND cm.deleted = false AND cm.action_type = 0
  AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
  AND cm.local_id > cp.available_min_id
  AND (cp.banned_until IS NULL OR cp.banned_until <= now());

-- name: ChannelMessageByLocal :one
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM channel_messages WHERE channel_id = $1 AND local_id = $2
  AND (expires_at IS NULL OR expires_at > statement_timestamp());

-- ChannelMessagesForDelete locks requested rows in local-id order. Callers
-- first hold channel_state and the caller participant SHARE lock.
-- name: ChannelMessagesForDelete :many
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM channel_messages
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND local_id = ANY(sqlc.arg(local_ids)::bigint[])
ORDER BY local_id
FOR UPDATE;

-- name: TombstoneChannelMessage :execrows
UPDATE channel_messages
SET deleted = true
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND local_id = sqlc.arg(local_id)::bigint
  AND deleted = false;

-- ExpiredChannelPostCandidates reads IDs without row locks. The caller takes
-- channel_state before rechecking and locking the posts, matching the normal
-- channel post mutation order.
-- name: ExpiredChannelPostCandidates :many
SELECT channel_id, local_id
FROM channel_messages
WHERE deleted = false
  AND action_type = 0
  AND expires_at <= statement_timestamp()
ORDER BY expires_at, channel_id, local_id
LIMIT sqlc.arg(lim)::int;

-- ExpiredChannelPostsForDelete rechecks eligibility under the channel state
-- lock and takes still-expired posts in local-id order.
-- name: ExpiredChannelPostsForDelete :many
SELECT post.local_id
FROM channel_messages AS post
WHERE post.channel_id = sqlc.arg(channel_id)::bigint
  AND post.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND post.deleted = false
  AND post.action_type = 0
  AND post.expires_at <= statement_timestamp()
ORDER BY post.local_id
FOR UPDATE OF post;

-- name: ChannelMessageByRandomID :one
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM channel_messages
WHERE channel_id = $1 AND random_id = $2 AND random_id <> 0
  AND (expires_at IS NULL OR expires_at > statement_timestamp());

-- ChannelMessageRandomIDExpired reports an expired dedup token without loading
-- the post contents, so a retry can be refused without replaying it.
-- name: ChannelMessageRandomIDExpired :one
SELECT EXISTS (
    SELECT 1 FROM channel_messages
    WHERE channel_id = sqlc.arg(channel_id)::bigint
      AND random_id = sqlc.arg(random_id)::bigint
      AND random_id <> 0
      AND expires_at <= statement_timestamp()
);

-- name: ChannelMessagesByLocalIDs :many
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM channel_messages
WHERE channel_id = $1 AND local_id = ANY(sqlc.arg(local_ids)::bigint[]) AND deleted = false
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
  AND EXISTS (SELECT 1 FROM channels c WHERE c.id = $1 AND c.deleted_at IS NULL);

-- ChannelMessagesForMember returns only live posts visible to this member. The
-- participant row supplies membership, ban state and the caller's own history
-- floor in the same statement as the post read.
-- name: ChannelMessagesForMember :many
SELECT cm.channel_id, cm.local_id, cm.from_id, cm.date, cm.message, cm.edit_date, cm.deleted,
       cm.random_id, cm.grouped_id, cm.file_id, cm.sticker_document_id, cm.reply_to_msg_id,
       cm.action_type, cm.ttl_period,
       cm.post_author, cm.entities
FROM channel_messages cm
JOIN channel_participants cp ON cp.channel_id = cm.channel_id AND cp.user_id = sqlc.arg(user_id)::bigint
WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
  AND cm.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND cm.deleted = false
  AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
  AND cm.local_id > cp.available_min_id
  AND (cp.banned_until IS NULL OR cp.banned_until <= now())
  AND EXISTS (
      SELECT 1 FROM channels c
      WHERE c.id = cm.channel_id AND c.deleted_at IS NULL
  );

-- ChannelMessageTombstonesForMember preserves MessageEmpty responses for
-- deleted ids while applying membership, ban and floor in the same statement.
-- It returns identity metadata only, without hydrating deleted content.
-- name: ChannelMessageTombstonesForMember :many
SELECT cm.channel_id, cm.local_id
FROM channel_messages cm
JOIN channel_participants cp ON cp.channel_id = cm.channel_id AND cp.user_id = sqlc.arg(user_id)::bigint
WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
  AND cm.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND cm.deleted = true
  AND cm.local_id > cp.available_min_id
  AND (cp.banned_until IS NULL OR cp.banned_until <= now())
  AND EXISTS (
      SELECT 1 FROM channels c
      WHERE c.id = cm.channel_id AND c.deleted_at IS NULL
  );

-- ChannelMessageTombstonesByLocalIDs returns only identity metadata for deleted
-- rows. Keeping this separate from the message load means a difference never
-- hydrates tombstoned text or its file reference.
-- name: ChannelMessageTombstonesByLocalIDs :many
SELECT channel_id, local_id
FROM channel_messages
WHERE channel_id = $1 AND local_id = ANY(sqlc.arg(local_ids)::bigint[]) AND deleted = true;

-- ChannelMessagesForForward is the authoritative source read for a channel
-- forward. Keep this lock after the participant SHARE lock and before file
-- reference locks. SKIP LOCKED makes an in-flight tombstone or edit fail closed
-- instead of forming a cycle with the eraser.
-- name: ChannelMessagesForForward :many
SELECT cm.channel_id, cm.local_id, cm.from_id, cm.date, cm.message, cm.edit_date, cm.deleted,
       cm.random_id, cm.grouped_id, cm.file_id, cm.sticker_document_id, cm.reply_to_msg_id,
       cm.action_type, cm.ttl_period,
       cm.post_author, cm.entities
FROM channel_messages cm
WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
  AND cm.local_id = ANY(sqlc.arg(local_ids)::bigint[])
  AND cm.deleted = false
  AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
  AND cm.action_type = 0
  AND EXISTS (
      SELECT 1 FROM channel_participants cp
      WHERE cp.channel_id = cm.channel_id
        AND cp.user_id = sqlc.arg(user_id)::bigint
        AND cm.local_id > cp.available_min_id
        AND (cp.banned_until IS NULL OR cp.banned_until <= now())
  )
  AND EXISTS (
      SELECT 1 FROM channels c
      WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
  )
ORDER BY cm.local_id
FOR SHARE OF cm SKIP LOCKED;

-- name: ChannelHistoryPage :many
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM channel_messages
WHERE channel_id = sqlc.arg(channel_id) AND deleted = false
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
  AND EXISTS (
      SELECT 1 FROM channels c
      WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
  )
  AND (sqlc.arg(offset_id)::bigint = 0 OR local_id < sqlc.arg(offset_id)::bigint)
ORDER BY local_id DESC
LIMIT sqlc.arg(lim)::int;

-- ChannelHistoryPageWithOffset uses a local_id seek for non-negative add_offset.
-- max_id and min_id filter the requested slice, matching Telegram's
-- messages.getHistory pagination order.
-- name: ChannelHistoryPageWithOffset :many
WITH page AS (
    SELECT cm.channel_id, cm.local_id, cm.from_id, cm.date, cm.message, cm.edit_date, cm.deleted,
           cm.random_id, cm.grouped_id, cm.file_id, cm.sticker_document_id, cm.reply_to_msg_id,
           cm.action_type, cm.ttl_period,
           cm.post_author, cm.entities
    FROM channel_messages cm
    JOIN channel_participants cp
      ON cp.channel_id = cm.channel_id
     AND cp.user_id = sqlc.arg(owner_id)::bigint
    WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
      AND cm.deleted = false
      AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
      AND cm.local_id > cp.available_min_id
      AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      AND EXISTS (
          SELECT 1 FROM channels c
          WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
      )
      AND (sqlc.arg(offset_id)::bigint = 0 OR cm.local_id < sqlc.arg(offset_id)::bigint)
    ORDER BY cm.local_id DESC
    LIMIT sqlc.arg(lim)::int
    OFFSET GREATEST(0::bigint, sqlc.arg(add_offset)::bigint)
)
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM page
WHERE (sqlc.arg(max_id)::bigint <= 0 OR local_id < sqlc.arg(max_id)::bigint)
  AND (sqlc.arg(min_id)::bigint <= 0 OR local_id > sqlc.arg(min_id)::bigint)
ORDER BY local_id DESC;

-- ChannelHistoryPageAround computes offset_id's ordinal only for negative
-- add_offset requests. max_id and min_id filter the resulting slice.
-- name: ChannelHistoryPageAround :many
WITH page_offset AS (
    SELECT GREATEST(
        COUNT(*) FILTER (WHERE cm.local_id >= sqlc.arg(offset_id)::bigint) + sqlc.arg(add_offset)::bigint,
        0::bigint
    ) AS skip
    FROM channel_messages cm
    JOIN channel_participants cp
      ON cp.channel_id = cm.channel_id
     AND cp.user_id = sqlc.arg(owner_id)::bigint
    WHERE cm.channel_id = sqlc.arg(channel_id)::bigint AND cm.deleted = false
      AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
      AND cm.local_id > cp.available_min_id
      AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      AND EXISTS (
          SELECT 1 FROM channels c
          WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
      )
), page AS (
    SELECT cm.channel_id, cm.local_id, cm.from_id, cm.date, cm.message, cm.edit_date, cm.deleted,
           cm.random_id, cm.grouped_id, cm.file_id, cm.sticker_document_id, cm.reply_to_msg_id,
           cm.action_type, cm.ttl_period,
           cm.post_author, cm.entities
    FROM channel_messages cm
    JOIN channel_participants cp
      ON cp.channel_id = cm.channel_id
     AND cp.user_id = sqlc.arg(owner_id)::bigint
    WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
      AND cm.deleted = false
      AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
      AND cm.local_id > cp.available_min_id
      AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      AND EXISTS (
          SELECT 1 FROM channels c
          WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
      )
      AND sqlc.arg(offset_id)::bigint >= 0
    ORDER BY cm.local_id DESC
    LIMIT sqlc.arg(lim)::int
    OFFSET (SELECT skip FROM page_offset)
)
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM page
WHERE (sqlc.arg(max_id)::bigint <= 0 OR local_id < sqlc.arg(max_id)::bigint)
  AND (sqlc.arg(min_id)::bigint <= 0 OR local_id > sqlc.arg(min_id)::bigint)
ORDER BY local_id DESC;

-- ChannelHistoryPageByDate resolves the date to the closest visible history
-- boundary, then seeks by local_id for the add_offset and result page.
-- name: ChannelHistoryPageByDate :many
WITH member AS MATERIALIZED (
    SELECT cp.channel_id, cp.available_min_id
    FROM channel_participants cp
    WHERE cp.channel_id = sqlc.arg(channel_id)::bigint
      AND cp.user_id = sqlc.arg(owner_id)::bigint
      AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      AND EXISTS (
          SELECT 1 FROM channels c
          WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
      )
), history_tail AS MATERIALIZED (
    -- Seek the latest row in each live stream; service rows with a TTL policy
    -- but no deadline use their partial local-ID index.
    SELECT candidates.local_id, candidates.date
    FROM (
        (
            SELECT candidate.local_id, candidate.date
            FROM member m
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = m.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id > m.available_min_id
                ORDER BY cm.local_id DESC
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM member m
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = m.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period <> 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id > m.available_min_id
                ORDER BY cm.local_id DESC
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM member m
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = m.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 86400
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id > m.available_min_id
                  AND cm.date > statement_timestamp() - make_interval(secs => 86400)
                ORDER BY cm.date DESC, cm.local_id DESC
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM member m
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = m.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 604800
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id > m.available_min_id
                  AND cm.date > statement_timestamp() - make_interval(secs => 604800)
                ORDER BY cm.date DESC, cm.local_id DESC
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM member m
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = m.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 2592000
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id > m.available_min_id
                  AND cm.date > statement_timestamp() - make_interval(secs => 2592000)
                ORDER BY cm.date DESC, cm.local_id DESC
                LIMIT 1
            ) AS candidate
        )
    ) AS candidates
    ORDER BY candidates.local_id DESC
    LIMIT 1
), floor_anchor AS MATERIALIZED (
    -- This primary-key seek establishes the member's date boundary without
    -- filtering through an expired backlog after the visibility floor.
    SELECT visible.local_id, visible.date, member.channel_id, member.available_min_id
    FROM member
    CROSS JOIN LATERAL (
        SELECT cm.local_id, cm.date
        FROM channel_messages cm
        WHERE cm.channel_id = member.channel_id
          AND cm.local_id > member.available_min_id
        ORDER BY cm.local_id
        LIMIT 1
    ) AS visible
), date_anchor AS MATERIALIZED (
    -- The insert trigger stamps expires_at as date + ttl_period. Splitting the
    -- allowed periods lets each date-index seek skip its expired backlog.
    SELECT candidates.local_id, candidates.date
    FROM (
        (
            SELECT candidate.local_id, candidate.date
            FROM floor_anchor fa
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = fa.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id > fa.available_min_id
                  AND cm.date >= GREATEST(
                      to_timestamp(sqlc.arg(offset_date)::bigint::double precision),
                      fa.date
                  )
                ORDER BY cm.date, cm.local_id
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM floor_anchor fa
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = fa.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period <> 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id > fa.available_min_id
                  AND cm.date >= GREATEST(
                      to_timestamp(sqlc.arg(offset_date)::bigint::double precision),
                      fa.date
                  )
                ORDER BY cm.date, cm.local_id
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM floor_anchor fa
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = fa.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 86400
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id > fa.available_min_id
                  AND cm.date >= GREATEST(
                      to_timestamp(sqlc.arg(offset_date)::bigint::double precision),
                      fa.date
                  )
                  AND cm.date > statement_timestamp() - make_interval(secs => 86400)
                ORDER BY cm.date, cm.local_id
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM floor_anchor fa
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = fa.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 604800
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id > fa.available_min_id
                  AND cm.date >= GREATEST(
                      to_timestamp(sqlc.arg(offset_date)::bigint::double precision),
                      fa.date
                  )
                  AND cm.date > statement_timestamp() - make_interval(secs => 604800)
                ORDER BY cm.date, cm.local_id
                LIMIT 1
            ) AS candidate
        )
        UNION ALL
        (
            SELECT candidate.local_id, candidate.date
            FROM floor_anchor fa
            CROSS JOIN LATERAL (
                SELECT cm.local_id, cm.date
                FROM channel_messages cm
                WHERE cm.channel_id = fa.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 2592000
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id > fa.available_min_id
                  AND cm.date >= GREATEST(
                      to_timestamp(sqlc.arg(offset_date)::bigint::double precision),
                      fa.date
                  )
                  AND cm.date > statement_timestamp() - make_interval(secs => 2592000)
                ORDER BY cm.date, cm.local_id
                LIMIT 1
            ) AS candidate
        )
    ) AS candidates
    ORDER BY candidates.date, candidates.local_id
    LIMIT 1
), cursor AS MATERIALIZED (
    SELECT COALESCE(
               (SELECT local_id FROM date_anchor),
               (SELECT local_id + 1 FROM history_tail)
           ) AS local_id,
           COALESCE(
               (SELECT date FROM date_anchor),
               (SELECT date FROM history_tail)
           ) AS date
), visible_starts AS MATERIALIZED (
    -- Stream marker -1 denotes TTL-change service rows with no expiry.
    -- For expiring posts, use the date index to jump over the expired prefix
    -- before using the local-id index to find the requested offset.
    SELECT m.channel_id, m.available_min_id, 0::integer AS ttl_period, cur.local_id
    FROM cursor cur
    JOIN member m ON true
    UNION ALL
    SELECT m.channel_id, m.available_min_id, -1::integer, first_visible.local_id
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period <> 0
          AND cm.expires_at IS NULL
          AND cm.local_id >= cur.local_id
          AND cm.local_id > m.available_min_id
        ORDER BY cm.local_id
        LIMIT 1
    ) AS first_visible
    UNION ALL
    SELECT m.channel_id, m.available_min_id, 86400::integer, first_visible.local_id
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period = 86400
          AND cm.expires_at > statement_timestamp()
          AND cm.local_id >= cur.local_id
          AND cm.local_id > m.available_min_id
          AND cm.date >= GREATEST(
              cur.date,
              statement_timestamp() - make_interval(secs => 86400)
          )
        ORDER BY cm.date, cm.local_id
        LIMIT 1
    ) AS first_visible
    UNION ALL
    SELECT m.channel_id, m.available_min_id, 604800::integer, first_visible.local_id
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period = 604800
          AND cm.expires_at > statement_timestamp()
          AND cm.local_id >= cur.local_id
          AND cm.local_id > m.available_min_id
          AND cm.date >= GREATEST(
              cur.date,
              statement_timestamp() - make_interval(secs => 604800)
          )
        ORDER BY cm.date, cm.local_id
        LIMIT 1
    ) AS first_visible
    UNION ALL
    SELECT m.channel_id, m.available_min_id, 2592000::integer, first_visible.local_id
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period = 2592000
          AND cm.expires_at > statement_timestamp()
          AND cm.local_id >= cur.local_id
          AND cm.local_id > m.available_min_id
          AND cm.date >= GREATEST(
              cur.date,
              statement_timestamp() - make_interval(secs => 2592000)
          )
        ORDER BY cm.date, cm.local_id
        LIMIT 1
    ) AS first_visible
), negative_page_start AS MATERIALIZED (
    SELECT candidates.local_id
    FROM (
        (
            SELECT candidate.local_id
            FROM visible_starts starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id >= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id
                LIMIT GREATEST(1::bigint, 0::bigint - sqlc.arg(add_offset)::bigint)
            ) AS candidate
            WHERE starts.ttl_period = 0
            ORDER BY candidate.local_id
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_starts starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period <> 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id >= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id
                LIMIT GREATEST(1::bigint, 0::bigint - sqlc.arg(add_offset)::bigint)
            ) AS candidate
            WHERE starts.ttl_period = -1
            ORDER BY candidate.local_id
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_starts starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 86400
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id >= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id
                LIMIT GREATEST(1::bigint, 0::bigint - sqlc.arg(add_offset)::bigint)
            ) AS candidate
            WHERE starts.ttl_period = 86400
            ORDER BY candidate.local_id
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_starts starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 604800
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id >= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id
                LIMIT GREATEST(1::bigint, 0::bigint - sqlc.arg(add_offset)::bigint)
            ) AS candidate
            WHERE starts.ttl_period = 604800
            ORDER BY candidate.local_id
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_starts starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 2592000
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id >= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id
                LIMIT GREATEST(1::bigint, 0::bigint - sqlc.arg(add_offset)::bigint)
            ) AS candidate
            WHERE starts.ttl_period = 2592000
            ORDER BY candidate.local_id
        )
    ) AS candidates
    ORDER BY candidates.local_id
    LIMIT 1
    OFFSET GREATEST(0::bigint, 0::bigint - sqlc.arg(add_offset)::bigint - 1)
), visible_ends AS MATERIALIZED (
    -- For positive offsets, seek each TTL period's nearest live older post
    -- by date before walking local IDs toward the requested page.
    SELECT m.channel_id, m.available_min_id, 0::integer AS ttl_period, cur.local_id, cur.date
    FROM cursor cur
    JOIN member m ON true
    UNION ALL
    SELECT m.channel_id, m.available_min_id, -1::integer, last_visible.local_id, last_visible.date
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id, cm.date
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period <> 0
          AND cm.expires_at IS NULL
          AND cm.local_id > m.available_min_id
          AND (cm.date, cm.local_id) < (cur.date, cur.local_id)
        ORDER BY cm.date DESC, cm.local_id DESC
        LIMIT 1
    ) AS last_visible
    UNION ALL
    SELECT m.channel_id, m.available_min_id, 86400::integer, last_visible.local_id, last_visible.date
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id, cm.date
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period = 86400
          AND cm.expires_at > statement_timestamp()
          AND cm.local_id > m.available_min_id
          AND (cm.date, cm.local_id) < (cur.date, cur.local_id)
          AND cm.date > statement_timestamp() - make_interval(secs => 86400)
        ORDER BY cm.date DESC, cm.local_id DESC
        LIMIT 1
    ) AS last_visible
    UNION ALL
    SELECT m.channel_id, m.available_min_id, 604800::integer, last_visible.local_id, last_visible.date
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id, cm.date
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period = 604800
          AND cm.expires_at > statement_timestamp()
          AND cm.local_id > m.available_min_id
          AND (cm.date, cm.local_id) < (cur.date, cur.local_id)
          AND cm.date > statement_timestamp() - make_interval(secs => 604800)
        ORDER BY cm.date DESC, cm.local_id DESC
        LIMIT 1
    ) AS last_visible
    UNION ALL
    SELECT m.channel_id, m.available_min_id, 2592000::integer, last_visible.local_id, last_visible.date
    FROM cursor cur
    JOIN member m ON true
    CROSS JOIN LATERAL (
        SELECT cm.local_id, cm.date
        FROM channel_messages cm
        WHERE cm.channel_id = m.channel_id
          AND cm.deleted = false
          AND cm.ttl_period = 2592000
          AND cm.expires_at > statement_timestamp()
          AND cm.local_id > m.available_min_id
          AND (cm.date, cm.local_id) < (cur.date, cur.local_id)
          AND cm.date > statement_timestamp() - make_interval(secs => 2592000)
        ORDER BY cm.date DESC, cm.local_id DESC
        LIMIT 1
    ) AS last_visible
), positive_page_start AS MATERIALIZED (
    SELECT candidates.local_id
    FROM (
        (
            SELECT candidate.local_id
            FROM visible_ends starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id < starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id DESC
                LIMIT GREATEST(1::bigint, sqlc.arg(add_offset)::bigint + 1)
            ) AS candidate
            WHERE starts.ttl_period = 0
            ORDER BY candidate.local_id DESC
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_ends starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period <> 0
                  AND cm.expires_at IS NULL
                  AND cm.local_id <= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id DESC
                LIMIT GREATEST(1::bigint, sqlc.arg(add_offset)::bigint + 1)
            ) AS candidate
            WHERE starts.ttl_period = -1
            ORDER BY candidate.local_id DESC
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_ends starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 86400
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id <= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id DESC
                LIMIT GREATEST(1::bigint, sqlc.arg(add_offset)::bigint + 1)
            ) AS candidate
            WHERE starts.ttl_period = 86400
            ORDER BY candidate.local_id DESC
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_ends starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 604800
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id <= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id DESC
                LIMIT GREATEST(1::bigint, sqlc.arg(add_offset)::bigint + 1)
            ) AS candidate
            WHERE starts.ttl_period = 604800
            ORDER BY candidate.local_id DESC
        )
        UNION ALL
        (
            SELECT candidate.local_id
            FROM visible_ends starts
            CROSS JOIN LATERAL (
                SELECT cm.local_id
                FROM channel_messages cm
                WHERE cm.channel_id = starts.channel_id
                  AND cm.deleted = false
                  AND cm.ttl_period = 2592000
                  AND cm.expires_at > statement_timestamp()
                  AND cm.local_id <= starts.local_id
                  AND cm.local_id > starts.available_min_id
                ORDER BY cm.local_id DESC
                LIMIT GREATEST(1::bigint, sqlc.arg(add_offset)::bigint + 1)
            ) AS candidate
            WHERE starts.ttl_period = 2592000
            ORDER BY candidate.local_id DESC
        )
    ) AS candidates
    ORDER BY candidates.local_id DESC
    LIMIT 1
    OFFSET sqlc.arg(add_offset)::bigint
), page_start AS (
    SELECT CASE
        WHEN sqlc.arg(add_offset)::bigint < 0 THEN COALESCE(
            (SELECT local_id FROM negative_page_start),
            (SELECT local_id FROM history_tail)
        )
        ELSE (SELECT local_id FROM positive_page_start)
    END AS local_id
), page AS (
    SELECT cm.channel_id, cm.local_id, cm.from_id, cm.date, cm.message, cm.edit_date, cm.deleted,
           cm.random_id, cm.grouped_id, cm.file_id, cm.sticker_document_id, cm.reply_to_msg_id,
           cm.action_type, cm.ttl_period, cm.post_author, cm.entities
    FROM channel_messages cm
    JOIN channel_participants cp
      ON cp.channel_id = cm.channel_id
     AND cp.user_id = sqlc.arg(owner_id)::bigint
    CROSS JOIN page_start
    WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
      AND cm.deleted = false
      AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
      AND cm.local_id > cp.available_min_id
      AND (cp.banned_until IS NULL OR cp.banned_until <= now())
      AND cm.local_id <= page_start.local_id
      AND EXISTS (
          SELECT 1 FROM channels c
          WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
      )
    ORDER BY cm.local_id DESC
    LIMIT sqlc.arg(lim)::int
)
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM page
WHERE (sqlc.arg(max_id)::bigint <= 0 OR local_id < sqlc.arg(max_id)::bigint)
  AND (sqlc.arg(min_id)::bigint <= 0 OR local_id > sqlc.arg(min_id)::bigint)
ORDER BY local_id DESC;

-- name: CountChannelHistory :one
-- Count content posts from the exact member-floor summary. Expired content
-- remains in that summary until the TTL sweeper marks it deleted, so subtract
-- currently expired posts through the expiry index. Service rows are sparse
-- relative to content posts and have their own partial history index.
WITH member AS MATERIALIZED (
    SELECT participant.available_min_id
    FROM channel_participants AS participant
    JOIN channels AS channel
      ON channel.id = participant.channel_id
     AND channel.deleted_at IS NULL
    WHERE participant.channel_id = sqlc.arg(channel_id)::bigint
      AND participant.user_id = sqlc.arg(owner_id)::bigint
      AND (participant.banned_until IS NULL OR participant.banned_until <= now())
), summary AS MATERIALIZED (
    SELECT result.entitled,
           result.status_exists,
           result.summary_version,
           result.summary_ready,
           result.total_live
    FROM member
    CROSS JOIN LATERAL channel_post_unread_suffix_counts(
        sqlc.arg(channel_id)::bigint,
        sqlc.arg(owner_id)::bigint,
        member.available_min_id
    ) AS result(entitled, status_exists, summary_version, summary_ready, total_live, author_live)
), service_count AS (
    SELECT count(*)::bigint AS count
    FROM member
    JOIN channel_messages AS service
      ON service.channel_id = sqlc.arg(channel_id)::bigint
     AND service.local_id > member.available_min_id
    WHERE service.deleted = false
      AND service.action_type <> 0
      AND (service.expires_at IS NULL OR service.expires_at > statement_timestamp())
), expired_post_count AS (
    SELECT count(*)::bigint AS count
    FROM member
    JOIN channel_messages AS expired
      ON expired.channel_id = sqlc.arg(channel_id)::bigint
     AND expired.local_id > member.available_min_id
    WHERE expired.deleted = false
      AND expired.action_type = 0
      AND expired.expires_at <= statement_timestamp()
)
SELECT summary.entitled::boolean AS entitled,
       summary.status_exists::boolean AS status_exists,
       summary.summary_version::smallint AS summary_version,
       summary.summary_ready::boolean AS summary_ready,
       summary.total_live::bigint AS total_live,
       COALESCE(service_count.count, 0)::bigint AS service_count,
       COALESCE(expired_post_count.count, 0)::bigint AS expired_post_count
FROM summary
CROSS JOIN service_count
CROSS JOIN expired_post_count;

-- ChannelRepliesPage reads a direct-reply page and its authorization snapshot
-- in one statement. Membership, ban state and floor gate the root, page and
-- count, so the result cannot outlive the caller's history visibility.
-- page_ids contains only local ids, allowing the reply index to serve the
-- ordered seek before the bounded rows are hydrated.
-- name: ChannelRepliesPage :many
WITH access AS (
    SELECT c.megagroup,
           cp.available_min_id,
           (cp.user_id IS NOT NULL AND (cp.banned_until IS NULL OR cp.banned_until <= now())) AS is_member
    FROM channels c
    LEFT JOIN channel_participants cp
      ON cp.channel_id = c.id
     AND cp.user_id = sqlc.arg(owner_id)::bigint
    WHERE c.id = sqlc.arg(channel_id)::bigint
), root AS (
    SELECT 1
    FROM channel_messages cm
    WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
      AND cm.local_id = sqlc.arg(root_id)::bigint
      AND cm.deleted = false
      AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
      AND cm.action_type = 0
      AND EXISTS (
          SELECT 1 FROM access
          WHERE is_member AND cm.local_id > available_min_id
      )
), eligible AS (
    SELECT cm.local_id
    FROM channel_messages cm
    WHERE cm.channel_id = sqlc.arg(channel_id)::bigint
      AND cm.reply_to_msg_id = sqlc.arg(root_id)::bigint
      AND cm.deleted = false
      AND (cm.expires_at IS NULL OR cm.expires_at > statement_timestamp())
      AND cm.action_type = 0
      AND EXISTS (
          SELECT 1 FROM access
          WHERE megagroup AND is_member AND cm.local_id > available_min_id
      )
      AND EXISTS (SELECT 1 FROM root)
), reply_count AS (
    SELECT count(*)::bigint AS total FROM eligible
), page_ids AS (
    SELECT local_id
    FROM eligible
    WHERE sqlc.arg(offset_id)::bigint = 0
       OR (sqlc.arg(offset_id)::bigint > 0 AND sqlc.arg(add_offset)::bigint < 0)
       OR local_id < sqlc.arg(offset_id)::bigint
    ORDER BY local_id DESC
    LIMIT sqlc.arg(lim)::int
    OFFSET CASE
        WHEN sqlc.arg(offset_id)::bigint > 0 AND sqlc.arg(add_offset)::bigint < 0 THEN
            GREATEST(
                0::bigint,
                (SELECT count(*) FROM eligible WHERE local_id >= sqlc.arg(offset_id)::bigint)
                    + sqlc.arg(add_offset)::bigint
            )
        ELSE GREATEST(0::bigint, sqlc.arg(add_offset)::bigint)
    END
), status AS (
    SELECT EXISTS (
               SELECT 1 FROM channels WHERE id = sqlc.arg(channel_id)::bigint
           ) AS channel_exists,
           EXISTS (
               SELECT 1 FROM channels
               WHERE id = sqlc.arg(channel_id)::bigint AND megagroup = true
           ) AS megagroup,
           EXISTS (SELECT 1 FROM access WHERE is_member) AS is_member,
           EXISTS (SELECT 1 FROM root) AS root_exists,
           (SELECT total FROM reply_count) AS reply_count
), selected AS (
    SELECT cm.channel_id,
           cm.local_id,
           cm.from_id,
           cm.date,
           cm.message,
           cm.edit_date,
           cm.deleted,
           cm.random_id,
           cm.grouped_id,
           cm.file_id,
           cm.sticker_document_id,
           cm.reply_to_msg_id,
           cm.action_type,
           cm.ttl_period,
           cm.entities
    FROM page_ids
    JOIN channel_messages cm
      ON cm.channel_id = sqlc.arg(channel_id)::bigint
     AND cm.local_id = page_ids.local_id
    WHERE (sqlc.arg(max_id)::bigint <= 0 OR cm.local_id < sqlc.arg(max_id)::bigint)
      AND (sqlc.arg(min_id)::bigint <= 0 OR cm.local_id > sqlc.arg(min_id)::bigint)
)
SELECT status.channel_exists,
       status.megagroup,
       status.is_member,
       status.root_exists,
       status.reply_count,
       selected.channel_id,
       selected.local_id,
       selected.from_id,
       selected.date,
       selected.message,
       selected.edit_date,
       selected.deleted,
       selected.random_id,
       selected.grouped_id,
       selected.file_id,
       selected.sticker_document_id,
       selected.reply_to_msg_id,
       selected.action_type,
       selected.ttl_period,
       selected.entities
FROM status
LEFT JOIN selected ON true
ORDER BY selected.local_id DESC;

-- SearchChannelPostsPage is ChannelHistoryPage narrowed by a full-text match.
-- The caller's membership, ban state and history floor are read from one
-- participant row in the same statement as the shared post content.
-- message_tsv is index-backed (GIN), so the match is not a sequential scan.
-- name: SearchChannelPostsPage :many
SELECT post.channel_id, post.local_id, post.from_id, post.date, post.message,
       post.edit_date, post.deleted, post.random_id, post.grouped_id, post.file_id,
       post.sticker_document_id,
       post.reply_to_msg_id, post.action_type, post.ttl_period, post.post_author, post.entities
FROM channel_messages post
JOIN channel_participants cp
  ON cp.channel_id = post.channel_id
 AND cp.user_id = sqlc.arg(owner_id)::bigint
WHERE post.channel_id = sqlc.arg(channel_id)::bigint AND post.deleted = false
  AND (post.expires_at IS NULL OR post.expires_at > statement_timestamp())
  AND post.local_id > cp.available_min_id
  AND (cp.banned_until IS NULL OR cp.banned_until <= now())
  AND EXISTS (
      SELECT 1 FROM channels c
      WHERE c.id = sqlc.arg(channel_id)::bigint AND c.deleted_at IS NULL
  )
  AND post.action_type = 0
  AND post.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query))
  AND (sqlc.arg(offset_id)::bigint = 0 OR post.local_id < sqlc.arg(offset_id)::bigint)
ORDER BY post.local_id DESC
LIMIT sqlc.arg(lim)::int;

-- Filtered shared-media channel searches keep membership, ban and floor in both
-- the count and page query. Channel posts are shared rows, so membership is the
-- authorized scope; an access hash alone never widens it. subtype_rights is the sender's
-- declared classification, not an authorization signal. Unknown (NULL),
-- generic and unstored files never match a subtype filter. The two file-backed
-- tabs are split on files.media_kind, exactly as the private-chat search does,
-- so a channel photo is counted and listed under Photos only and any other
-- stored file under Files only. Polls stay unmatched here: a channel poll has
-- no per-viewer poll copy, and the store answers that filter without
-- reading posts.
-- name: CountFilteredChannelPosts :one
SELECT count(*)::bigint
FROM channel_messages post
WHERE post.channel_id = sqlc.arg(channel_id)::bigint
  AND post.deleted = false
  AND (post.expires_at IS NULL OR post.expires_at > statement_timestamp())
  AND post.action_type = 0
  AND EXISTS (
      SELECT 1 FROM channel_participants cp
      WHERE cp.channel_id = post.channel_id
        AND cp.user_id = sqlc.arg(owner_id)::bigint
        AND post.local_id > cp.available_min_id
        AND (cp.banned_until IS NULL OR cp.banned_until <= now())
  )
  AND CASE sqlc.arg(filter)::smallint
      WHEN 1 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.media_kind = 'document'
      )
      WHEN 2 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.media_kind = 'photo'
      )
      WHEN 3 THEN post.message ~* '(^|[^[:alnum:]_@])(([[:alpha:]][[:alnum:]+.-]*://|www[.])[^[:space:]]+|[[:alnum:]-]+[.][[:alpha:]]{2,}(:[0-9]{1,5})?(/[[:graph:]]*)?)' -- noqa: LT05
      WHEN 4 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_videos']::text[]
      )
      WHEN 5 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_gifs']::text[]
      )
      WHEN 6 THEN false
      WHEN 7 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights && ARRAY['send_roundvideos', 'send_voices']::text[]
      )
      WHEN 8 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_audios']::text[]
      )
      WHEN 9 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
      )
      ELSE false
  END
  AND (sqlc.arg(query)::text = '' OR post.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query)));

-- name: SearchFilteredChannelPostsPage :many
SELECT post.channel_id, post.local_id, post.from_id, post.date, post.message,
       post.edit_date, post.deleted, post.random_id, post.grouped_id,
       post.file_id, post.sticker_document_id, post.reply_to_msg_id,
       post.action_type, post.ttl_period, post.post_author, post.entities
FROM channel_messages post
WHERE post.channel_id = sqlc.arg(channel_id)::bigint
  AND post.deleted = false
  AND (post.expires_at IS NULL OR post.expires_at > statement_timestamp())
  AND post.action_type = 0
  AND EXISTS (
      SELECT 1 FROM channel_participants cp
      WHERE cp.channel_id = post.channel_id
        AND cp.user_id = sqlc.arg(owner_id)::bigint
        AND post.local_id > cp.available_min_id
        AND (cp.banned_until IS NULL OR cp.banned_until <= now())
  )
  AND CASE sqlc.arg(filter)::smallint
      WHEN 1 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.media_kind = 'document'
      )
      WHEN 2 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.media_kind = 'photo'
      )
      WHEN 3 THEN post.message ~* '(^|[^[:alnum:]_@])(([[:alpha:]][[:alnum:]+.-]*://|www[.])[^[:space:]]+|[[:alnum:]-]+[.][[:alpha:]]{2,}(:[0-9]{1,5})?(/[[:graph:]]*)?)' -- noqa: LT05
      WHEN 4 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_videos']::text[]
      )
      WHEN 5 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_gifs']::text[]
      )
      WHEN 6 THEN false
      WHEN 7 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights && ARRAY['send_roundvideos', 'send_voices']::text[]
      )
      WHEN 8 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND f.subtype_rights @> ARRAY['send_audios']::text[]
      )
      WHEN 9 THEN post.file_id IS NOT NULL AND EXISTS (
          SELECT 1 FROM files f WHERE f.id = post.file_id AND f.stored = true
            AND (f.media_kind = 'photo' OR f.subtype_rights @> ARRAY['send_videos']::text[])
      )
      ELSE false
  END
  AND (sqlc.arg(query)::text = '' OR post.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query)))
  AND (sqlc.arg(offset_id)::bigint = 0 OR post.local_id < sqlc.arg(offset_id)::bigint)
ORDER BY post.local_id DESC
LIMIT sqlc.arg(lim)::int;

-- SearchPinnedChannelPostForMember returns the active pinned post only while
-- the viewer has an unbanned participant row and the pin is above its history
-- floor. Posts are shared, so unlike chat pins the channel local_id is the wire id.
-- name: SearchPinnedChannelPostForMember :many
SELECT post.channel_id, post.local_id, post.from_id, post.date, post.message,
       post.edit_date, post.deleted, post.random_id, post.grouped_id,
       post.file_id, post.sticker_document_id, post.reply_to_msg_id,
       post.action_type, post.ttl_period, post.post_author, post.entities
FROM channels c
JOIN channel_participants participant
  ON participant.channel_id = c.id
 AND participant.user_id = sqlc.arg(owner_id)::bigint
 AND (participant.banned_until IS NULL OR participant.banned_until <= now())
JOIN channel_messages post
  ON post.channel_id = c.id
 AND post.local_id = c.pinned_message_id
 AND post.deleted = false
 AND (post.expires_at IS NULL OR post.expires_at > statement_timestamp())
 AND post.action_type = 0
  AND post.local_id > participant.available_min_id
WHERE c.id = sqlc.arg(channel_id)::bigint
  AND c.deleted_at IS NULL
  AND c.pinned_message_id IS NOT NULL
  AND (sqlc.arg(query)::text = '' OR post.message_tsv @@ plainto_tsquery('simple', sqlc.arg(query)))
  AND (sqlc.arg(offset_id)::bigint = 0 OR post.local_id < sqlc.arg(offset_id)::bigint)
ORDER BY post.local_id DESC
LIMIT sqlc.arg(lim)::int;
