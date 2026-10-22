-- name: InstallStickerSet :execrows
INSERT INTO user_sticker_sets (user_id, set_id)
VALUES ($1, $2)
ON CONFLICT (user_id, set_id) DO NOTHING;

-- name: UninstallStickerSet :execrows
DELETE FROM user_sticker_sets
WHERE user_id = $1 AND set_id = $2;

-- StickerSetsByOwner is ordered by the table's composite primary key and only
-- returns install state for the authenticated account named by the caller.
-- name: StickerSetsByOwner :many
SELECT set_id, installed_at
FROM user_sticker_sets
WHERE user_id = $1
ORDER BY set_id;
