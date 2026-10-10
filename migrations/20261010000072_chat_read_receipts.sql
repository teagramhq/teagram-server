-- First-read times for eligible basic-group message copies. Existing messages
-- and read markers remain authoritative; legacy reads have no receipt rows.
CREATE TABLE chat_read_receipts (
    chat_id   BIGINT      NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    fanout_id BIGINT      NOT NULL CHECK (fanout_id <> 0),
    reader_id BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    sent_at   TIMESTAMPTZ NOT NULL,
    read_at   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (chat_id, fanout_id, reader_id)
);

CREATE INDEX chat_read_receipts_retention_idx
    ON chat_read_receipts (sent_at, chat_id, fanout_id, reader_id);
