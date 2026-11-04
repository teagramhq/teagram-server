-- Active-item reads should stay bounded as removed item history grows.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY user_created_sticker_set_items_active_position_idx
    ON user_created_sticker_set_items (set_id, position)
    WHERE is_active = true;
