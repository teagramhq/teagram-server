-- Serve both exact expiry scans in group history from one index range on
-- expiry: expired posts (expires_at <= now) or unexpired posts (expires_at > now),
-- each bounded to the rows it counts. Build concurrently so channel writes stay
-- available.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_expiring_post_idx
    ON channel_messages (channel_id, expires_at)
    INCLUDE (local_id)
    WHERE deleted = false AND action_type = 0 AND expires_at IS NOT NULL;
