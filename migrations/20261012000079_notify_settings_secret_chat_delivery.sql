ALTER TABLE notify_settings_difference_cursors
    ADD COLUMN secret_chat_acknowledged JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN secret_chat_issued JSONB NOT NULL DEFAULT '[]'::jsonb;
