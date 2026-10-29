-- Support per-inviter rolling quota counts and bounded newest-first lists.
-- Build concurrently so the history index does not block invite writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY registration_invites_inviter_issued_at_idx
    ON registration_invites (inviter_user_id, issued_at DESC, id DESC);
