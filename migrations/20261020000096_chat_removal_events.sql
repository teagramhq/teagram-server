-- Chat removal updates are durable state changes, but they do not occupy PTS.
-- Keep the event for cross-replica live fanout and a separately consumable
-- marker for updates.getDifference.
CREATE TABLE chat_removal_events (
    owner_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chat_id  BIGINT NOT NULL REFERENCES chats (id),
    version  INT    NOT NULL,
    PRIMARY KEY (owner_id, chat_id)
);

CREATE TABLE chat_removal_state_markers (
    owner_id BIGINT NOT NULL,
    chat_id  BIGINT NOT NULL,
    PRIMARY KEY (owner_id, chat_id),
    FOREIGN KEY (owner_id, chat_id)
        REFERENCES chat_removal_events (owner_id, chat_id) ON DELETE CASCADE
);
