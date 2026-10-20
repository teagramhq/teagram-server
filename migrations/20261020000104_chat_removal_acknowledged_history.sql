CREATE OR REPLACE FUNCTION preserve_chat_removal_client_state_history()
RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.difference_state_date IS NOT NULL
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
