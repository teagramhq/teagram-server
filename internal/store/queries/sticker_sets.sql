-- name: InstallStickerSet :execrows
INSERT INTO user_sticker_sets (user_id, set_id)
VALUES ($1, $2)
ON CONFLICT (user_id, set_id) DO NOTHING;

-- name: UninstallStickerSet :execrows
DELETE FROM user_sticker_sets
WHERE user_id = $1 AND set_id = $2;

-- StickerSetsByOwner returns install state for the authenticated account named
-- by the caller, in its saved order. Sets without a saved position follow the
-- placed sets in set_id order.
-- name: StickerSetsByOwner :many
SELECT set_id, installed_at
FROM user_sticker_sets
WHERE user_id = $1
ORDER BY sort_position ASC NULLS LAST, set_id;

-- SetUserStickerSetOrder writes the caller's full installed order in one
-- statement: set_ids lists every installed set in its new order, and each
-- position is that set's 1-based index.
-- name: SetUserStickerSetOrder :execrows
UPDATE user_sticker_sets AS sticker_sets
SET sort_position = ordered.position::int
FROM unnest(sqlc.arg(set_ids)::bigint[]) WITH ORDINALITY AS ordered(set_id, position)
WHERE sticker_sets.user_id = sqlc.arg(user_id)
  AND sticker_sets.set_id = ordered.set_id;
