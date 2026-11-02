-- Build the substring-search index without blocking sticker-set writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY user_created_sticker_sets_live_title_trgm_idx
    ON public.user_created_sticker_sets USING gin (lower(title) gin_trgm_ops)
    WHERE is_live = true;
