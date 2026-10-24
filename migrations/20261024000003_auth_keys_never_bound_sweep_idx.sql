-- The bounded sweep walks only keys with no binding history, oldest last-seen
-- first. Keep its order and filter in a partial index; the batch delete is a
-- frequent concurrent operation on a live auth-key table.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY auth_keys_never_bound_sweep_idx
    ON auth_keys (last_seen_at, id)
    WHERE user_id IS NULL AND ever_bound_at IS NULL;
