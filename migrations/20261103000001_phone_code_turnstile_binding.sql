-- Preserve the Siteverify action and auth key for a challenge accepted by
-- auth.sendCode. Existing rows and calls without an enforced challenge remain
-- NULL; the new application code can roll back while the additive columns stay.
-- The new action column is NULL for existing rows, so NOT VALID avoids a scan;
-- PostgreSQL still enforces the constraint on new writes.
ALTER TABLE phone_codes
    ADD COLUMN turnstile_action TEXT NULL,
    ADD COLUMN turnstile_auth_key_id BIGINT NULL,
    ADD CONSTRAINT phone_codes_turnstile_action_check
        CHECK (turnstile_action IN ('signin', 'signup')) NOT VALID;
