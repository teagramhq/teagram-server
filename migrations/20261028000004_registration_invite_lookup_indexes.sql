-- Index invite issuance history lookups without blocking live invite writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY registration_invites_inviter_user_id_idx
    ON registration_invites (inviter_user_id)
    WHERE inviter_user_id IS NOT NULL;
