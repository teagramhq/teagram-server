-- Index literal short-name substrings that contain separators, which pg_trgm
-- cannot prune in their original text representation.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY user_created_sticker_sets_live_short_name_literal_trgm_idx
    ON public.user_created_sticker_sets USING gin (public.user_sticker_set_search_key(lower(short_name)) gin_trgm_ops)
    WHERE is_live = true;
