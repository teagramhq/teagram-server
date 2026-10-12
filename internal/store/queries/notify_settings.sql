-- name: NotifySettingsForKey :one
SELECT show_previews, silent, mute_until, sound, changed_at
FROM user_notify_settings
WHERE owner_id = sqlc.arg(owner_id)
  AND scope = sqlc.arg(scope)
  AND peer_type = sqlc.arg(peer_type)
  AND peer_id = sqlc.arg(peer_id);

-- NotifySettingsForVisibleKey checks peer visibility without the mutation locks,
-- in the same statement as the owner-private setting read.
-- name: NotifySettingsForVisibleKey :one
WITH requested AS (
    SELECT sqlc.arg(owner_id)::bigint AS owner_id,
           sqlc.arg(scope)::smallint AS scope,
           sqlc.arg(peer_type)::smallint AS peer_type,
           sqlc.arg(peer_id)::bigint AS peer_id
), visibility AS (
    SELECT requested.*,
           requested.scope <> 1 OR (
               (requested.peer_type = 1 AND EXISTS (
                   SELECT 1 FROM dialogs AS dialog
                   WHERE dialog.owner_id = requested.owner_id
                     AND dialog.peer_type = requested.peer_type
                     AND dialog.peer_id = requested.peer_id
               ))
               OR (requested.peer_type = 2 AND EXISTS (
                   SELECT 1 FROM chat_participants AS participant
                   WHERE participant.chat_id = requested.peer_id
                     AND participant.user_id = requested.owner_id
               ))
               OR (requested.peer_type = 3 AND EXISTS (
                   SELECT 1 FROM channel_participants AS participant
                   WHERE participant.channel_id = requested.peer_id
                     AND participant.user_id = requested.owner_id
                     AND (participant.banned_until IS NULL OR participant.banned_until <= now())
               ))
           ) AS peer_visible
    FROM requested
)
SELECT COALESCE(visibility.peer_visible, false)::boolean AS peer_visible,
       (setting.owner_id IS NOT NULL)::boolean AS found,
       setting.show_previews,
       setting.silent,
       setting.mute_until,
       setting.sound
FROM visibility
LEFT JOIN user_notify_settings AS setting
  ON setting.owner_id = visibility.owner_id
 AND setting.scope = visibility.scope
 AND setting.peer_type = visibility.peer_type
 AND setting.peer_id = visibility.peer_id
 AND visibility.peer_visible;

-- name: UpsertNotifySettings :exec
INSERT INTO user_notify_settings (
    owner_id, scope, peer_type, peer_id, show_previews, silent, mute_until, sound, changed_at
)
VALUES (
    sqlc.arg(owner_id), sqlc.arg(scope), sqlc.arg(peer_type), sqlc.arg(peer_id),
    sqlc.narg(show_previews), sqlc.narg(silent), sqlc.narg(mute_until), sqlc.narg(sound), sqlc.arg(changed_at)
)
ON CONFLICT (owner_id, scope, peer_type, peer_id) DO UPDATE
SET show_previews = EXCLUDED.show_previews,
    silent = EXCLUDED.silent,
    mute_until = EXCLUDED.mute_until,
    sound = EXCLUDED.sound,
    changed_at = EXCLUDED.changed_at;

-- name: NotifySettingsTimestamp :one
SELECT clock_timestamp()::timestamptz AS changed_at;

-- name: PruneNotifySettingsForOwner :exec
DELETE FROM user_notify_settings AS setting
WHERE setting.owner_id = sqlc.arg(owner_id)
  AND setting.scope = 1
  AND NOT (
      (setting.peer_type = 1 AND EXISTS (
          SELECT 1 FROM dialogs AS dialog
          WHERE dialog.owner_id = setting.owner_id
            AND dialog.peer_type = setting.peer_type
            AND dialog.peer_id = setting.peer_id
      ))
      OR (setting.peer_type = 2 AND EXISTS (
          SELECT 1 FROM chat_participants AS participant
          WHERE participant.chat_id = setting.peer_id
            AND participant.user_id = setting.owner_id
      ))
      OR (setting.peer_type = 3 AND EXISTS (
          SELECT 1 FROM channel_participants AS participant
          WHERE participant.channel_id = setting.peer_id
            AND participant.user_id = setting.owner_id
            AND (participant.banned_until IS NULL OR participant.banned_until <= now())
      ))
  );

-- name: CountActivePeerNotifySettings :one
SELECT count(*)
FROM user_notify_settings
WHERE owner_id = sqlc.arg(owner_id)
  AND scope = 1
  AND (show_previews IS NOT NULL OR silent IS NOT NULL OR mute_until IS NOT NULL OR sound IS NOT NULL);

-- name: NotifyNotifySettings :exec
SELECT pg_notify('tg_updates', sqlc.arg(payload)::text);
