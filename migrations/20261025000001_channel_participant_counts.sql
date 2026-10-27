-- Keep exact channel membership counts in a row that does not share either
-- the channel metadata lock or the channel post sequence lock.
CREATE TABLE channel_participant_counts (
    channel_id BIGINT PRIMARY KEY REFERENCES channels (id) ON DELETE CASCADE,
    participant_count BIGINT NOT NULL DEFAULT 0
);

CREATE FUNCTION initialize_channel_participant_count()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO channel_participant_counts (channel_id, participant_count)
    VALUES (NEW.id, 0);
    RETURN NEW;
END;
$$;

CREATE TRIGGER channels_participant_count_after_insert
AFTER INSERT ON channels
FOR EACH ROW
EXECUTE FUNCTION initialize_channel_participant_count();

CREATE FUNCTION maintain_channel_participant_count()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO channel_participant_counts (channel_id, participant_count)
        VALUES (NEW.channel_id, 1)
        ON CONFLICT (channel_id) DO UPDATE
        SET participant_count = channel_participant_counts.participant_count + 1;
        RETURN NEW;
    END IF;

    UPDATE channel_participant_counts
    SET participant_count = participant_count - 1
    WHERE channel_id = OLD.channel_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'missing participant count for channel %', OLD.channel_id;
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER channel_participants_count_after_insert_or_delete
AFTER INSERT OR DELETE ON channel_participants
FOR EACH ROW
EXECUTE FUNCTION maintain_channel_participant_count();

CREATE FUNCTION move_channel_participant_count()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.channel_id IS DISTINCT FROM OLD.channel_id THEN
        UPDATE channel_participant_counts
        SET participant_count = participant_count - 1
        WHERE channel_id = OLD.channel_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'missing participant count for channel %', OLD.channel_id;
        END IF;

        INSERT INTO channel_participant_counts (channel_id, participant_count)
        VALUES (NEW.channel_id, 1)
        ON CONFLICT (channel_id) DO UPDATE
        SET participant_count = channel_participant_counts.participant_count + 1;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER channel_participants_count_after_channel_change
AFTER UPDATE OF channel_id ON channel_participants
FOR EACH ROW
EXECUTE FUNCTION move_channel_participant_count();

-- Seed rows for existing channels. The following migration backfills the exact
-- values in committed batches while these triggers preserve concurrent changes.
INSERT INTO channel_participant_counts (channel_id)
SELECT id FROM channels;
