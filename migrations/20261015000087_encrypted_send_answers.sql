-- Every new secret-chat send records its dedup key and the date the sender was
-- answered, whether it was delivered or withheld from a recipient who blocked
-- the sender. encrypted_events rows are deleted once the recipient acks them,
-- so this table is what keeps a resend's answer the same in both cases, and
-- nothing in it says which case it was. No recipient read path touches it.
CREATE TABLE encrypted_send_answers (
    recipient_id BIGINT NOT NULL REFERENCES users(id),
    random_id    BIGINT NOT NULL,
    date         TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (recipient_id, random_id)
);
