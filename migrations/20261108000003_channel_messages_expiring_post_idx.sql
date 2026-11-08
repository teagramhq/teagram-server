-- Serve the unexpired expiring-post count in group history from an index range
-- on expiry, skipping expired rows the sweeper has not tombstoned yet. Build
-- concurrently so channel writes stay available.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_expiring_post_idx
    ON channel_messages (channel_id, expires_at)
    INCLUDE (local_id)
    WHERE deleted = false AND action_type = 0 AND expires_at IS NOT NULL;
