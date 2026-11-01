-- The partial index supports thumbnail file-reference lookups without blocking set writes.
-- atlas:txmode none

CREATE UNIQUE INDEX CONCURRENTLY user_created_sticker_sets_thumb_file_key
    ON user_created_sticker_sets (thumb_file_id)
    WHERE thumb_file_id IS NOT NULL;
