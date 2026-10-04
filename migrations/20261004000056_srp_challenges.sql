-- SRP challenge secrets are encrypted by the store before they reach this table.
-- The unique auth-key/user pair preserves one active challenge per login key.
CREATE TABLE srp_challenges (
    srp_id      BIGINT PRIMARY KEY CHECK (srp_id <> 0),
    auth_key_id BIGINT NOT NULL REFERENCES auth_keys (id) ON DELETE CASCADE,
    user_id     BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    b_secret    BYTEA  NOT NULL,
    b_public    BYTEA  NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (auth_key_id, user_id)
);

CREATE INDEX srp_challenges_expires_at_idx ON srp_challenges (expires_at);
