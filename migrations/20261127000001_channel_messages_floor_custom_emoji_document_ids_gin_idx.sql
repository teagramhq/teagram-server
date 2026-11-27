-- Bound channel emoji history lookups by member visibility floors.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_floor_custom_emoji_document_ids_gin_idx
    ON channel_messages USING GIN (
        channel_id,
        local_id,
        message_custom_emoji_document_ids(entities)
    )
    WHERE deleted = false
      AND entities IS NOT NULL
      AND cardinality(message_custom_emoji_document_ids(entities)) > 0;
