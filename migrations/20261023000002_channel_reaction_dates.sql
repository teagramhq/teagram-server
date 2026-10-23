ALTER TABLE channel_message_reactions
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
