-- CommonChatsForUsers returns current basic-group and readable supergroup
-- memberships for both users. The total is computed before max_id paging so it
-- is stable across pages and matches users.getFullUser.common_chats_count.
-- Broadcast channels are excluded; an active ban excludes a supergroup member
-- because that member cannot read the channel today.
-- name: CommonChatsForUsers :many
WITH common_chats AS (
    SELECT c.id,
           2::smallint AS peer_type,
           c.title,
           c.creator_id,
           c.version,
           c.date,
           c.default_banned_rights,
           count(all_members.user_id)::bigint AS participant_count,
           NULL::text AS username,
           0::smallint AS member_role
    FROM chats AS c
    JOIN chat_participants AS viewer
      ON viewer.chat_id = c.id AND viewer.user_id = sqlc.arg(viewer_id)::bigint
    JOIN chat_participants AS target
      ON target.chat_id = c.id AND target.user_id = sqlc.arg(target_id)::bigint
    JOIN chat_participants AS all_members ON all_members.chat_id = c.id
    GROUP BY c.id

    UNION ALL

    SELECT c.id,
           3::smallint AS peer_type,
           c.title,
           c.creator_id,
           c.version,
           c.date,
           c.default_banned_rights,
           0::bigint AS participant_count,
           c.username,
           viewer.role AS member_role
    FROM channels AS c
    JOIN channel_participants AS viewer
      ON viewer.channel_id = c.id
     AND viewer.user_id = sqlc.arg(viewer_id)::bigint
     AND (viewer.banned_until IS NULL OR viewer.banned_until <= now())
    JOIN channel_participants AS target
      ON target.channel_id = c.id
     AND target.user_id = sqlc.arg(target_id)::bigint
     AND (target.banned_until IS NULL OR target.banned_until <= now())
    WHERE c.megagroup = true
), counted AS (
    SELECT common_chats.*, count(*) OVER()::bigint AS total_count
    FROM common_chats
), candidate_page AS (
    -- Keep every peer with the last candidate's id on the same page.
    SELECT id
    FROM counted
    WHERE sqlc.arg(max_id)::bigint <= 0 OR id < sqlc.arg(max_id)::bigint
    ORDER BY id DESC, peer_type DESC
    LIMIT sqlc.arg(lim)::int
), page AS (
    SELECT counted.*
    FROM counted
    WHERE (sqlc.arg(max_id)::bigint <= 0 OR id < sqlc.arg(max_id)::bigint)
      AND id >= (SELECT MIN(id) FROM candidate_page)
), results AS (
    SELECT page.*, false AS is_empty FROM page
    UNION ALL
    SELECT 0::bigint AS id,
           0::smallint AS peer_type,
           ''::text AS title,
           0::bigint AS creator_id,
           0::integer AS version,
           'epoch'::timestamptz AS date,
           '{}'::text[] AS default_banned_rights,
           0::bigint AS participant_count,
           NULL::text AS username,
           0::smallint AS member_role,
           COALESCE(MAX(total_count), 0)::bigint AS total_count,
           true AS is_empty
    FROM counted
    WHERE NOT EXISTS (SELECT 1 FROM page)
)
SELECT id, peer_type, title, creator_id, version, date,
       default_banned_rights, participant_count, username, member_role,
       total_count
FROM results
ORDER BY is_empty, id DESC, peer_type DESC;
