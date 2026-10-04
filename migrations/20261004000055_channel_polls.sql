-- Channel messages have one shared row rather than per-member copies. Keep a
-- separate link to the canonical poll so channel membership does not multiply
-- poll storage or voter state.
CREATE TABLE channel_poll_messages (
    channel_id BIGINT NOT NULL,
    local_id   BIGINT NOT NULL,
    poll_id    BIGINT NOT NULL UNIQUE REFERENCES polls (id) ON DELETE CASCADE,
    PRIMARY KEY (channel_id, local_id),
    FOREIGN KEY (channel_id, local_id)
        REFERENCES channel_messages (channel_id, local_id) ON DELETE CASCADE
);

CREATE INDEX channel_poll_messages_poll_idx ON channel_poll_messages (poll_id);
