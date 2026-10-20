ALTER TABLE chat_removal_client_state
    ADD COLUMN difference_state_pts BIGINT,
    ADD COLUMN difference_state_qts BIGINT,
    ADD COLUMN difference_state_date BIGINT,
    ADD COLUMN resume_date BIGINT;

CREATE INDEX chat_removal_client_state_difference_state_idx
    ON chat_removal_client_state (owner_id, auth_key_id, difference_state_date)
    WHERE acknowledged_at IS NULL AND difference_state_date IS NOT NULL;
