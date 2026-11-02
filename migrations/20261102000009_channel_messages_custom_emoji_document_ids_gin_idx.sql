-- Support custom-emoji document-reference lookups in channel message history.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_custom_emoji_document_ids_gin_idx
    ON channel_messages USING GIN (message_custom_emoji_document_ids(entities))
    WHERE deleted = false AND entities IS NOT NULL;
