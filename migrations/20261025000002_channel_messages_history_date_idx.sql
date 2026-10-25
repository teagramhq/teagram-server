-- Channel history resolves date anchors by channel and date. Build concurrently
-- so posting remains available while the index is added.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_history_date_idx
    ON channel_messages (channel_id, date, local_id)
    WHERE deleted = false;
