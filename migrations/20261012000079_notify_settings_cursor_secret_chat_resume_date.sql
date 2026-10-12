ALTER TABLE notify_settings_difference_cursors
    ADD COLUMN secret_chat_resume_date BIGINT NOT NULL DEFAULT 0;
