ALTER TABLE bot_tokens
    ADD COLUMN update_offset BIGINT NOT NULL DEFAULT 0
        CHECK (update_offset >= 0);
