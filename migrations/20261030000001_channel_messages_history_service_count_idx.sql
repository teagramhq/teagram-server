-- Count active channel service rows through a bounded history index scan.
-- Build concurrently so the additive index does not block channel writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_history_service_count_idx
    ON channel_messages (channel_id, local_id)
    INCLUDE (expires_at)
    WHERE deleted = false AND action_type <> 0;
