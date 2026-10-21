ALTER TABLE messages
    ADD COLUMN ttl_period INTEGER NOT NULL DEFAULT 0
        CHECK (ttl_period IN (0, 86400, 604800, 2592000)),
    ADD COLUMN expires_at TIMESTAMPTZ NULL;

ALTER TABLE chats
    ADD COLUMN ttl_period INTEGER NOT NULL DEFAULT 0
        CHECK (ttl_period IN (0, 86400, 604800, 2592000));

ALTER TABLE channels
    ADD COLUMN ttl_period INTEGER NOT NULL DEFAULT 0
        CHECK (ttl_period IN (0, 86400, 604800, 2592000));

ALTER TABLE channel_messages
    ADD COLUMN ttl_period INTEGER NOT NULL DEFAULT 0
        CHECK (ttl_period IN (0, 86400, 604800, 2592000)),
    ADD COLUMN expires_at TIMESTAMPTZ NULL;

CREATE TABLE user_dialog_history_ttls (
    user_low_id  BIGINT NOT NULL REFERENCES users (id),
    user_high_id BIGINT NOT NULL REFERENCES users (id),
    ttl_period   INTEGER NOT NULL CHECK (ttl_period IN (0, 86400, 604800, 2592000)),
    PRIMARY KEY (user_low_id, user_high_id),
    CHECK (user_low_id < user_high_id)
);

CREATE FUNCTION stamp_message_history_ttl() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    period INTEGER := 0;
BEGIN
    IF NEW.action_type <> 0 THEN
        RETURN NEW;
    END IF;

    IF NEW.peer_type = 1 THEN
        SELECT ttl_period INTO period
        FROM user_dialog_history_ttls
        WHERE user_low_id = LEAST(NEW.owner_id, NEW.peer_id)
          AND user_high_id = GREATEST(NEW.owner_id, NEW.peer_id);
    ELSIF NEW.peer_type = 2 THEN
        SELECT ttl_period INTO period FROM chats WHERE id = NEW.peer_id;
    END IF;

    IF COALESCE(period, 0) > 0 THEN
        NEW.ttl_period := period;
        NEW.expires_at := NEW.date + make_interval(secs => period);
    ELSE
        NEW.ttl_period := 0;
        NEW.expires_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER messages_stamp_history_ttl
BEFORE INSERT ON messages
FOR EACH ROW EXECUTE FUNCTION stamp_message_history_ttl();

CREATE FUNCTION stamp_channel_message_history_ttl() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    period INTEGER := 0;
BEGIN
    IF NEW.action_type <> 0 THEN
        RETURN NEW;
    END IF;

    SELECT ttl_period INTO period FROM channels WHERE id = NEW.channel_id;
    IF COALESCE(period, 0) > 0 THEN
        NEW.ttl_period := period;
        NEW.expires_at := NEW.date + make_interval(secs => period);
    ELSE
        NEW.ttl_period := 0;
        NEW.expires_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER channel_messages_stamp_history_ttl
BEFORE INSERT ON channel_messages
FOR EACH ROW EXECUTE FUNCTION stamp_channel_message_history_ttl();
