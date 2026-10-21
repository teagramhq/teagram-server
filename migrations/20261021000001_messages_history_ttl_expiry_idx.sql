-- Keep each bounded expiry scan on the live rows that carry a deadline. The
-- concurrent build avoids blocking message writes; rollback drops only this
-- additive index.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY messages_history_ttl_expiry_idx
    ON messages (expires_at, owner_id, local_id)
    WHERE deleted = false AND expires_at IS NOT NULL;
