-- Scope channel poll custom-emoji lookups by channel and requested document IDs.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_poll_messages_channel_custom_emoji_document_ids_gin_idx
    ON channel_poll_messages USING GIN (channel_id, custom_emoji_document_ids)
    WHERE cardinality(custom_emoji_document_ids) > 0;
