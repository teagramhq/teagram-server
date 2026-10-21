-- Keep each bounded channel-post expiry scan on content posts that carry a
-- deadline. The concurrent build avoids blocking channel writes; rollback
-- drops only this additive index.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_history_ttl_expiry_idx
    ON channel_messages (expires_at, channel_id, local_id)
    WHERE deleted = false AND action_type = 0 AND expires_at IS NOT NULL;
