CREATE TABLE user_sticker_sets (
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    set_id       BIGINT NOT NULL,
    installed_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (user_id, set_id)
);
