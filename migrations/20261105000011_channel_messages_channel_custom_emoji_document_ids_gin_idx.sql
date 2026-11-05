-- Scope channel custom-emoji history lookups by channel and document ID.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_channel_custom_emoji_document_ids_gin_idx
    ON channel_messages USING GIN (channel_id, message_custom_emoji_document_ids(entities))
    WHERE deleted = false
      AND entities IS NOT NULL
      AND cardinality(message_custom_emoji_document_ids(entities)) > 0;
