CREATE TABLE channel_available_reactions (
    channel_id BIGINT PRIMARY KEY REFERENCES channels (id) ON DELETE CASCADE,
    mode       TEXT NOT NULL CHECK (mode IN ('all', 'some', 'none')),
    reactions  TEXT[] NOT NULL DEFAULT '{}',
    CHECK (mode = 'some' OR cardinality(reactions) = 0)
);

CREATE TABLE channel_message_reactions (
    channel_id BIGINT NOT NULL,
    local_id   BIGINT NOT NULL,
    reactor_id BIGINT NOT NULL,
    reaction   TEXT NOT NULL CHECK (char_length(reaction) BETWEEN 1 AND 32),
    PRIMARY KEY (channel_id, local_id, reactor_id),
    FOREIGN KEY (channel_id, local_id)
        REFERENCES channel_messages (channel_id, local_id) ON DELETE CASCADE
);
