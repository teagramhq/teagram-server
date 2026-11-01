-- Seek TTL-change service rows by date without scanning expiring posts; build
-- concurrently to preserve channel writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_history_ttl_service_date_idx
    ON channel_messages (channel_id, date, local_id)
    WHERE deleted = false AND ttl_period <> 0 AND expires_at IS NULL;
