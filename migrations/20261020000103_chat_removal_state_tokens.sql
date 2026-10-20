-- Serialize PTS-neutral removal Date reservations per authorization key.
CREATE TABLE chat_removal_difference_state_tokens (
    owner_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    auth_key_id BIGINT NOT NULL,
    last_state_date BIGINT NOT NULL,
    PRIMARY KEY (owner_id, auth_key_id)
);
