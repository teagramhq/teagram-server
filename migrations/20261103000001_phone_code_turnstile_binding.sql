-- Preserve the Siteverify action and auth key for a challenge accepted by
-- auth.sendCode. Existing rows and calls without an enforced challenge remain
-- NULL; the new application code can roll back while the additive columns stay.
ALTER TABLE phone_codes
    ADD COLUMN turnstile_action TEXT NULL,
    ADD COLUMN turnstile_auth_key_id BIGINT NULL;
