-- Bound channel history offsets by TTL period and message ID. The concurrent
-- build keeps channel posting available while the index is added.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_history_ttl_local_idx
    ON channel_messages (channel_id, ttl_period, local_id)
    INCLUDE (expires_at)
    WHERE deleted = false;
