-- Keep basic-group descriptions outside chats so older binaries' SELECT *
-- projections keep their existing row shape during rollback.
-- Existing chats intentionally have no row and therefore read as empty.
CREATE TABLE chat_about (
    chat_id BIGINT PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    about   TEXT NOT NULL
);
