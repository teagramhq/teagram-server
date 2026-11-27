-- Probe channel custom-emoji history once for a set of visible channels.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_visible_channels_custom_emoji_gin_idx
    ON channel_messages USING GIN ((ARRAY[channel_id]), message_custom_emoji_document_ids(entities))
    WHERE deleted = false
      AND entities IS NOT NULL
      AND cardinality(message_custom_emoji_document_ids(entities)) > 0;
