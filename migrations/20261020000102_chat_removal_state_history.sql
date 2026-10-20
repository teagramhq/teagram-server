CREATE TABLE chat_removal_client_state_history (
    owner_id BIGINT NOT NULL,
    auth_key_id BIGINT NOT NULL,
    chat_id BIGINT NOT NULL,
    difference_state_pts BIGINT NOT NULL,
    difference_state_qts BIGINT NOT NULL,
    difference_state_date BIGINT NOT NULL,
    resume_date BIGINT NOT NULL,
    PRIMARY KEY (owner_id, auth_key_id, chat_id, difference_state_date),
    FOREIGN KEY (owner_id, auth_key_id, chat_id)
        REFERENCES chat_removal_client_state (owner_id, auth_key_id, chat_id) ON DELETE CASCADE
);

CREATE INDEX chat_removal_client_state_history_date_idx
    ON chat_removal_client_state_history (owner_id, auth_key_id, difference_state_date);

CREATE FUNCTION preserve_chat_removal_client_state_history()
RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.acknowledged_at IS NULL
       AND OLD.difference_state_date IS NOT NULL
       AND NEW.difference_state_date IS DISTINCT FROM OLD.difference_state_date THEN
        INSERT INTO chat_removal_client_state_history (
            owner_id, auth_key_id, chat_id, difference_state_pts,
            difference_state_qts, difference_state_date, resume_date
        ) VALUES (
            OLD.owner_id, OLD.auth_key_id, OLD.chat_id, OLD.difference_state_pts,
            OLD.difference_state_qts, OLD.difference_state_date, OLD.resume_date
        )
        ON CONFLICT (owner_id, auth_key_id, chat_id, difference_state_date) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER chat_removal_client_state_archive
AFTER UPDATE ON chat_removal_client_state
FOR EACH ROW
EXECUTE FUNCTION preserve_chat_removal_client_state_history();
