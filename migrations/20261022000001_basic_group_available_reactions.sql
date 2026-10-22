CREATE TABLE chat_available_reactions (
    chat_id   BIGINT PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    mode      TEXT NOT NULL CHECK (mode IN ('all', 'some', 'none')),
    reactions TEXT[] NOT NULL DEFAULT '{}',
    CHECK (mode = 'some' OR cardinality(reactions) = 0)
);
