ALTER TABLE bot_tokens
    ADD COLUMN poll_generation BIGINT NOT NULL DEFAULT 0
        CHECK (poll_generation >= 0);
