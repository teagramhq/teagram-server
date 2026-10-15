-- A secret-chat message to a recipient who has blocked the sender is never
-- stored as an encrypted event. Only its dedup key and the date the sender was
-- answered are kept, so a resend after an unblock returns that answer and
-- delivers nothing. No recipient read path touches this table.
CREATE TABLE withheld_encrypted_messages (
    recipient_id BIGINT NOT NULL REFERENCES users(id),
    random_id    BIGINT NOT NULL,
    date         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (recipient_id, random_id)
);
