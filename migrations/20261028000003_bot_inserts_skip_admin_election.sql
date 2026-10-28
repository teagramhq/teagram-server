-- Bots do not consume the first-human server administrator election.
CREATE OR REPLACE FUNCTION ensure_server_administration_closed()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.kind = 'bot' THEN
        RETURN NEW;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM server_administration
        WHERE singleton_id = 1
          AND election_closed
    ) THEN
        RAISE EXCEPTION 'server administrator election is open';
    END IF;
    RETURN NEW;
END;
$$;
