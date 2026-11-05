-- Keep each poll copy's custom emoji IDs beside its visibility scope.
ALTER TABLE poll_message_copies
    ADD COLUMN custom_emoji_document_ids TEXT[] NOT NULL DEFAULT '{}'::text[];

ALTER TABLE channel_poll_messages
    ADD COLUMN custom_emoji_document_ids TEXT[] NOT NULL DEFAULT '{}'::text[];

CREATE FUNCTION set_poll_message_copy_custom_emoji_document_ids()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    SELECT message_custom_emoji_document_ids(poll.description_entities)
      INTO NEW.custom_emoji_document_ids
      FROM polls AS poll
     WHERE poll.id = NEW.poll_id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER poll_message_copies_custom_emoji_document_ids
BEFORE INSERT ON poll_message_copies
FOR EACH ROW
EXECUTE FUNCTION set_poll_message_copy_custom_emoji_document_ids();

CREATE FUNCTION set_channel_poll_custom_emoji_document_ids()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    SELECT message_custom_emoji_document_ids(poll.description_entities)
      INTO NEW.custom_emoji_document_ids
      FROM polls AS poll
     WHERE poll.id = NEW.poll_id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER channel_poll_messages_custom_emoji_document_ids
BEFORE INSERT ON channel_poll_messages
FOR EACH ROW
EXECUTE FUNCTION set_channel_poll_custom_emoji_document_ids();

CREATE FUNCTION refresh_poll_copy_custom_emoji_document_ids()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    UPDATE poll_message_copies AS copy
       SET custom_emoji_document_ids = message_custom_emoji_document_ids(NEW.description_entities)
     WHERE copy.poll_id = NEW.id;

    UPDATE channel_poll_messages AS copy
       SET custom_emoji_document_ids = message_custom_emoji_document_ids(NEW.description_entities)
     WHERE copy.poll_id = NEW.id;

    RETURN NEW;
END;
$$;

CREATE TRIGGER polls_custom_emoji_document_ids_refresh
AFTER UPDATE OF description_entities ON polls
FOR EACH ROW
WHEN (OLD.description_entities IS DISTINCT FROM NEW.description_entities)
EXECUTE FUNCTION refresh_poll_copy_custom_emoji_document_ids();
