-- OwnedUploadGrantFileIDs filters client-named file ids down to the ones the
-- caller holds a grant for, without locking anything. An album send uses it
-- before it takes the files-row interlock, so a reference to another account's
-- file is refused without the caller ever locking that account's row.
-- name: OwnedUploadGrantFileIDs :many
SELECT file_id FROM file_upload_grants
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND file_id = ANY(sqlc.arg(file_ids)::bigint[]);

-- LockUploadGrants row-locks the caller's named grants in ascending file_id
-- order. Albums from one caller to different channels share no other lock, so
-- this order, not the planner's row order in ConsumeUploadGrants, is what
-- keeps two albums naming the same grants from each holding one and waiting
-- for the other. A grant consumed by a transaction this one waited on is
-- skipped, and the caller treats the shortfall as a refusal.
-- name: LockUploadGrants :many
SELECT file_id FROM file_upload_grants
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND file_id = ANY(sqlc.arg(file_ids)::bigint[])
ORDER BY file_id
FOR UPDATE;

-- ConsumeUploadGrants deletes every named grant the caller owns that is still
-- inside its lifetime and whose stored file carries the presented access hash.
-- The caller compares the returned rows with what it asked for and rolls the
-- whole transaction back on any shortfall, so a refused album consumes nothing.
-- name: ConsumeUploadGrants :many
WITH consumed AS (
    DELETE FROM file_upload_grants AS g
    USING files AS f,
          (SELECT unnest(sqlc.arg(file_ids)::bigint[]) AS file_id,
                  unnest(sqlc.arg(access_hashes)::bigint[]) AS access_hash) AS ref
    WHERE g.file_id = ref.file_id
      AND f.id = g.file_id
      AND f.access_hash = ref.access_hash
      AND f.stored = true
      AND g.owner_id = sqlc.arg(owner_id)::bigint
      AND g.created_at >= sqlc.arg(not_before)::timestamptz
    RETURNING g.file_id
)
SELECT files.id, files.media_kind, files.subtype_rights
FROM files JOIN consumed ON consumed.file_id = files.id;

-- BumpStateBy allocates n consecutive local ids and n consecutive pts values
-- for one owner. It refuses, like BumpState, any allocation that would leave
-- the 32-bit wire range.
-- name: BumpStateBy :one
UPDATE update_state
SET pts = pts + sqlc.arg(n)::bigint, next_local_id = next_local_id + sqlc.arg(n)::bigint, date = now()
WHERE user_id = sqlc.arg(user_id)::bigint
  AND pts <= 2147483647 - sqlc.arg(n)::bigint
  AND next_local_id <= 2147483648 - sqlc.arg(n)::bigint
RETURNING pts, (next_local_id - 1)::bigint AS local_id;

-- InsertAlbumMessages writes every owner copy of every album item in one
-- statement. The arrays are parallel, one element per row.
-- name: InsertAlbumMessages :exec
INSERT INTO messages (owner_id, local_id, peer_type, peer_id, from_id, message, out, random_id, peer_local_id,
                      fanout_id, file_id, grouped_id, entities)
SELECT unnest(sqlc.arg(owner_ids)::bigint[]),
       unnest(sqlc.arg(local_ids)::bigint[]),
       sqlc.arg(peer_type)::smallint,
       unnest(sqlc.arg(peer_ids)::bigint[]),
       sqlc.arg(from_id)::bigint,
       unnest(sqlc.arg(texts)::text[]),
       unnest(sqlc.arg(outs)::boolean[]),
       unnest(sqlc.arg(random_ids)::bigint[]),
       unnest(sqlc.arg(peer_local_ids)::bigint[]),
       unnest(sqlc.arg(fanout_ids)::bigint[]),
       unnest(sqlc.arg(file_ids)::bigint[]),
       sqlc.arg(grouped_id)::bigint,
       NULLIF(unnest(sqlc.arg(entities)::text[]), '')::jsonb;

-- InsertNewMessageEvents writes one new-message event per album row.
-- name: InsertNewMessageEvents :exec
INSERT INTO message_events (owner_id, pts, type, local_id)
SELECT unnest(sqlc.arg(owner_ids)::bigint[]),
       unnest(sqlc.arg(pts)::bigint[]),
       sqlc.arg(type)::smallint,
       unnest(sqlc.arg(local_ids)::bigint[]);

-- UpsertDialogs is UpsertDialog for many owners of one peer kind.
-- name: UpsertDialogs :exec
INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message, unread_count)
SELECT unnest(sqlc.arg(owner_ids)::bigint[]),
       sqlc.arg(peer_type)::smallint,
       unnest(sqlc.arg(peer_ids)::bigint[]),
       unnest(sqlc.arg(top_messages)::bigint[]),
       unnest(sqlc.arg(unread_counts)::int[])
ON CONFLICT (owner_id, peer_type, peer_id) DO UPDATE
  SET top_message  = EXCLUDED.top_message,
      unread_count = dialogs.unread_count + EXCLUDED.unread_count;

-- MessagesByRandomIDs returns the caller's own rows carrying any of the
-- presented dedup tokens.
-- name: MessagesByRandomIDs :many
SELECT * FROM messages
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND random_id = ANY(sqlc.arg(random_ids)::bigint[])
  AND random_id <> 0
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
ORDER BY local_id;

-- ExpiredMessageRandomIDs returns only dedup tokens, never the expired rows'
-- content, so an album retry can be refused before it loads any stale item.
-- name: ExpiredMessageRandomIDs :many
SELECT random_id FROM messages
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND random_id = ANY(sqlc.arg(random_ids)::bigint[])
  AND random_id <> 0
  AND expires_at <= statement_timestamp()
ORDER BY random_id;

-- CountGroupedMessagesInRange counts an owner's rows of one album. An album's
-- rows occupy consecutive local ids, so the caller bounds the scan to the
-- primary-key range around the rows it already holds.
-- name: CountGroupedMessagesInRange :one
SELECT count(*)::bigint FROM messages
WHERE owner_id = sqlc.arg(owner_id)::bigint
  AND local_id BETWEEN sqlc.arg(min_local_id)::bigint AND sqlc.arg(max_local_id)::bigint
  AND grouped_id = sqlc.arg(grouped_id)::bigint;

-- BumpChannelStateBy is BumpStateBy for a channel's shared log.
-- name: BumpChannelStateBy :one
UPDATE channel_state
SET pts = pts + sqlc.arg(n)::bigint, next_local_id = next_local_id + sqlc.arg(n)::bigint, date = now()
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND pts <= 2147483647 - sqlc.arg(n)::bigint
  AND next_local_id <= 2147483648 - sqlc.arg(n)::bigint
RETURNING pts, (next_local_id - 1)::bigint AS local_id;

-- InsertChannelAlbumMessages writes every post of one channel album.
-- name: InsertChannelAlbumMessages :exec
INSERT INTO channel_messages (
    channel_id, local_id, from_id, message, random_id, file_id, reply_to_msg_id, grouped_id, post_author, entities
)
SELECT sqlc.arg(channel_id)::bigint,
       unnest(sqlc.arg(local_ids)::bigint[]),
       sqlc.arg(from_id)::bigint,
       unnest(sqlc.arg(texts)::text[]),
       unnest(sqlc.arg(random_ids)::bigint[]),
       unnest(sqlc.arg(file_ids)::bigint[]),
       sqlc.narg(reply_to_msg_id)::int,
       sqlc.arg(grouped_id)::bigint,
       sqlc.narg(post_author)::text,
       NULLIF(unnest(sqlc.arg(entities)::text[]), '')::jsonb;

-- InsertChannelNewMessageEvents writes one new-message event per album post.
-- name: InsertChannelNewMessageEvents :exec
INSERT INTO channel_events (channel_id, pts, type, local_id)
SELECT sqlc.arg(channel_id)::bigint,
       unnest(sqlc.arg(pts)::bigint[]),
       sqlc.arg(type)::smallint,
       unnest(sqlc.arg(local_ids)::bigint[]);

-- ChannelMessagesByRandomIDs returns the posts carrying any of the presented
-- dedup tokens in this channel's own random_id space.
-- name: ChannelMessagesByRandomIDs :many
SELECT channel_id, local_id, from_id, date, message, edit_date, deleted,
       random_id, grouped_id, file_id, sticker_document_id, reply_to_msg_id,
       action_type, ttl_period, post_author, entities
FROM channel_messages
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND random_id = ANY(sqlc.arg(random_ids)::bigint[])
  AND random_id <> 0
  AND (expires_at IS NULL OR expires_at > statement_timestamp())
ORDER BY local_id;

-- ExpiredChannelMessageRandomIDs returns only dedup tokens, never expired post
-- content, so an album retry can be refused before it loads any stale item.
-- name: ExpiredChannelMessageRandomIDs :many
SELECT random_id FROM channel_messages
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND random_id = ANY(sqlc.arg(random_ids)::bigint[])
  AND random_id <> 0
  AND expires_at <= statement_timestamp()
ORDER BY random_id;

-- CountGroupedChannelMessagesInRange is CountGroupedMessagesInRange for a
-- channel album.
-- name: CountGroupedChannelMessagesInRange :one
SELECT count(*)::bigint FROM channel_messages
WHERE channel_id = sqlc.arg(channel_id)::bigint
  AND local_id BETWEEN sqlc.arg(min_local_id)::bigint AND sqlc.arg(max_local_id)::bigint
  AND grouped_id = sqlc.arg(grouped_id)::bigint;
