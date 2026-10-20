-- Direct reply lookup pages are user-facing reads over the hot channel message
-- table. Build the additive index concurrently so the initial scan does not
-- block channel posts; rollback drops only this index.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_replies_idx
    ON channel_messages (channel_id, reply_to_msg_id, local_id)
    WHERE deleted = false;
