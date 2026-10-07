-- name: ListDialogPins :many
SELECT owner_id, peer_type, peer_id, position
FROM user_dialog_pins
WHERE owner_id = $1 AND peer_type <> 0
ORDER BY position;

-- name: DialogPinDialogsForOwner :many
SELECT peer_type, peer_id
FROM dialogs
WHERE owner_id = $1
  AND peer_type = ANY(sqlc.arg(peer_types)::smallint[])
  AND peer_id = ANY(sqlc.arg(peer_ids)::bigint[]);

-- name: InsertDialogPin :exec
INSERT INTO user_dialog_pins (owner_id, peer_type, peer_id, position)
VALUES ($1, $2, $3, $4);

-- name: DeleteDialogPinsForOwner :exec
DELETE FROM user_dialog_pins
WHERE owner_id = $1 AND peer_type <> 0;

-- name: MarkDialogPinsChanged :exec
INSERT INTO user_dialog_pins (owner_id, peer_type, peer_id, position, changed_at)
VALUES ($1, 0, 0, NULL, clock_timestamp())
ON CONFLICT (owner_id, peer_type, peer_id)
DO UPDATE SET changed_at = clock_timestamp();

-- name: DialogPinChangeAt :one
SELECT changed_at
FROM user_dialog_pins
WHERE owner_id = $1 AND peer_type = 0 AND peer_id = 0;

-- name: NotifyDialogPinMutation :exec
SELECT pg_notify('tg_dialog_pins', $1);

-- EnsureSelfDialogForOwner creates the empty Saved Messages dialog on its
-- first pin without advancing message pts.
-- name: EnsureSelfDialogForOwner :exec
INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message, unread_count)
VALUES ($1, 1, $1, 0, 0)
ON CONFLICT (owner_id, peer_type, peer_id) DO NOTHING;

-- DeleteEmptySelfDialogForOwner removes only the synthetic Saved Messages row
-- after its final pin is removed; a real conversation remains untouched.
-- name: DeleteEmptySelfDialogForOwner :exec
DELETE FROM dialogs
WHERE owner_id = $1 AND peer_type = 1 AND peer_id = $1 AND top_message = 0;
