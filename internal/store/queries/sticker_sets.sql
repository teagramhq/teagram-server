-- name: InstallStickerSet :execrows
INSERT INTO user_sticker_sets (user_id, set_id)
VALUES ($1, $2)
ON CONFLICT (user_id, set_id) DO NOTHING;

-- name: UninstallStickerSet :execrows
DELETE FROM user_sticker_sets
WHERE user_id = $1 AND set_id = $2;

-- StickerSetsByOwner follows the account's chosen order. Sets without a
-- position, which is every set until the account reorders, keep set_id order
-- after the positioned ones. It only returns install state for the account
-- named by the caller.
-- name: StickerSetsByOwner :many
SELECT set_id, installed_at
FROM user_sticker_sets
WHERE user_id = $1
ORDER BY sort_position NULLS LAST, set_id;

-- SetInstalledStickerSetPositions gives each listed set its 1-based position in
-- the array order. The caller passes every installed set id in one statement.
-- name: SetInstalledStickerSetPositions :execrows
UPDATE user_sticker_sets AS u
SET sort_position = o.pos::INT
FROM unnest(sqlc.arg(set_ids)::bigint[]) WITH ORDINALITY AS o(set_id, pos)
WHERE u.user_id = sqlc.arg(user_id) AND u.set_id = o.set_id;
