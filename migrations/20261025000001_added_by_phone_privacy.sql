-- Added-by-phone settings share the existing rule storage with status privacy.
-- Widen the key constraint while preserving all existing status rules.
ALTER TABLE user_privacy_rules
    DROP CONSTRAINT user_privacy_rules_privacy_key_check,
    ADD CONSTRAINT user_privacy_rules_privacy_key_check
        CHECK (privacy_key IN ('status_timestamp', 'added_by_phone'));
