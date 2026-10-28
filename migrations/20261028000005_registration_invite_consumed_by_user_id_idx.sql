-- Index redeemer attribution lookups without blocking live invite writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY registration_invites_consumed_by_user_id_idx
    ON registration_invites (consumed_by_user_id)
    WHERE consumed_by_user_id IS NOT NULL;
