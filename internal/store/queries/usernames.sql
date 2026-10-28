-- name: GetUsernameByHandle :one
SELECT handle, owner_type, owner_id
FROM usernames
WHERE handle = lower(sqlc.arg(handle))
  AND owner_type IN ('user', 'channel');

-- UsernameHandleTaken includes active channel holds without making a held
-- channel handle resolvable to its tombstoned channel row.
-- name: UsernameHandleTaken :one
SELECT EXISTS (
    SELECT 1
    FROM usernames un
    WHERE un.handle = lower(sqlc.arg(handle))
      AND (
          un.owner_type IN ('user', 'channel')
          OR (un.owner_type = 'channel_hold' AND un.hold_until > clock_timestamp())
      )
);

-- name: ClaimUsername :one
INSERT INTO usernames (handle, owner_type, owner_id)
VALUES (lower(sqlc.arg(handle)), sqlc.arg(owner_type), sqlc.arg(owner_id))
ON CONFLICT (handle) DO UPDATE
SET owner_type = EXCLUDED.owner_type,
    owner_id = EXCLUDED.owner_id,
    hold_creator_id = NULL,
    hold_until = NULL
WHERE usernames.owner_type = 'channel_hold'
  AND usernames.hold_until <= clock_timestamp()
RETURNING handle, owner_type, owner_id;

-- ClaimHeldChannelUsername transfers an active held handle only to a channel
-- created after the deletion by the same creator account, in one statement.
-- name: ClaimHeldChannelUsername :execrows
UPDATE usernames AS held_username
SET owner_type = 'channel',
    owner_id = sqlc.arg(channel_id)::bigint,
    hold_creator_id = NULL,
    hold_until = NULL
WHERE held_username.handle = lower(sqlc.arg(handle))
  AND held_username.owner_type = 'channel_hold'
  AND held_username.hold_creator_id = sqlc.arg(creator_id)::bigint
  AND held_username.hold_until > clock_timestamp()
  AND EXISTS (
      SELECT 1
      FROM channels AS deleted_channel
      JOIN channels AS target_channel
        ON target_channel.id = sqlc.arg(channel_id)::bigint
      WHERE deleted_channel.id = held_username.owner_id
        AND deleted_channel.deleted_at IS NOT NULL
        AND target_channel.deleted_at IS NULL
        AND target_channel.creator_id = held_username.hold_creator_id
        AND target_channel.date > deleted_channel.deleted_at
  );

-- name: ReleaseUsername :execrows
DELETE FROM usernames
WHERE handle = lower(sqlc.arg(handle))
  AND owner_type = sqlc.arg(owner_type)
  AND owner_id = sqlc.arg(owner_id);

-- name: ReleaseUsernameByOwner :execrows
DELETE FROM usernames WHERE owner_type = sqlc.arg(owner_type) AND owner_id = sqlc.arg(owner_id);

-- name: GetUserByUsername :one
-- The handle reported back is the joined usernames row — the one that admitted
-- the account to this result — not the denormalized users.username copy.
SELECT u.id, u.phone, u.first_name, u.last_name, u.created_at, u.is_online, u.last_seen_at,
       un.handle AS username
FROM users u
JOIN usernames un ON un.owner_type = 'user' AND un.owner_id = u.id
WHERE un.handle = lower(sqlc.arg(handle));

-- name: GetUserByUsernameWithLoginMode :one
-- Resolves a normalized username to the owning user, including login_mode and kind.
-- Only matches owner_type='user' — a channel with the same handle returns no row.
-- The signIn handler refuses bot accounts before password lookup or pending staging.
SELECT u.id, u.phone, u.first_name, u.last_name, u.created_at, u.is_online, u.last_seen_at,
       u.login_mode, u.kind, un.handle AS username
FROM users u
JOIN usernames un ON un.owner_type = 'user' AND un.owner_id = u.id
WHERE un.handle = lower(sqlc.arg(handle));

-- name: GetChannelByUsername :one
SELECT c.* FROM channels c
JOIN usernames un ON un.owner_type = 'channel' AND un.owner_id = c.id
WHERE un.handle = lower(sqlc.arg(handle)) AND c.deleted_at IS NULL;
