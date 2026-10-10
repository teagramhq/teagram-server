-- First-read times for eligible basic-group message copies. Existing messages
-- and read markers remain authoritative; legacy reads have no receipt rows.
-- chat_id intentionally has no FK: receipt writes hold owner locks, so checking
-- chats would invert the chat-row-before-owner lock order used by sends.
CREATE TABLE chat_read_receipts (
    chat_id   BIGINT      NOT NULL,
    fanout_id BIGINT      NOT NULL CHECK (fanout_id <> 0),
    reader_id BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    sent_at   TIMESTAMPTZ NOT NULL,
    read_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (chat_id, fanout_id, reader_id)
);

CREATE INDEX chat_read_receipts_retention_idx
    ON chat_read_receipts (sent_at, chat_id, fanout_id, reader_id);
