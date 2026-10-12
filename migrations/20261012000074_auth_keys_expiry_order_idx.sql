-- The authorization sweep selects bound sessions oldest first in bounded
-- batches. Keep that order in an index so each pass can stop after its batch
-- instead of sorting the remaining joined sessions. Build concurrently because
-- authenticated frames update last_seen_at while sessions remain active.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY auth_keys_bound_last_seen_id_idx
    ON auth_keys (last_seen_at, id)
    WHERE user_id IS NOT NULL;
