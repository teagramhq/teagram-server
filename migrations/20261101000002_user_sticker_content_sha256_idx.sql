-- Build concurrently to keep sticker-set writes available during the index scan.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY user_created_sticker_set_items_content_sha256_idx
    ON user_created_sticker_set_items (content_sha256)
    WHERE content_sha256 IS NOT NULL;
