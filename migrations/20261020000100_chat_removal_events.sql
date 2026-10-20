-- Chat removal updates are durable state changes, but they do not occupy PTS.
-- Keep the event for cross-replica live fanout and per-authorization
-- acknowledgements across updates.getDifference and updates.getState.
CREATE TABLE chat_removal_events (
    owner_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chat_id  BIGINT NOT NULL REFERENCES chats (id),
    version  INT    NOT NULL,
    PRIMARY KEY (owner_id, chat_id)
);

CREATE TABLE chat_removal_client_state (
    owner_id BIGINT NOT NULL,
    chat_id  BIGINT NOT NULL,
    auth_key_id BIGINT NOT NULL,
    delivered_at TIMESTAMPTZ NOT NULL,
    acknowledged_at TIMESTAMPTZ NULL,
    PRIMARY KEY (owner_id, auth_key_id, chat_id),
    FOREIGN KEY (owner_id, chat_id)
        REFERENCES chat_removal_events (owner_id, chat_id) ON DELETE CASCADE
);
