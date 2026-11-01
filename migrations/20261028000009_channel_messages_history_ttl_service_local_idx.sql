-- Keep TTL-change service rows in local-ID history seeks without walking
-- expired posts; build concurrently to preserve channel writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_history_ttl_service_local_idx
    ON channel_messages (channel_id, local_id)
    WHERE deleted = false AND ttl_period <> 0 AND expires_at IS NULL;
