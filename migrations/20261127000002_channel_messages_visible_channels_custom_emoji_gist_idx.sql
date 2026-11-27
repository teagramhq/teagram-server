-- Probe visible channel history and exact per-channel history floors in one scan.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_visible_channels_custom_emoji_gist_idx
    ON channel_messages USING GIST (
        public.channel_message_custom_emoji_location(channel_id, local_id),
        public.message_custom_emoji_document_vector(entities) tsvector_ops (siglen = 512)
    )
    WHERE deleted = false
      AND entities IS NOT NULL
      AND cardinality(message_custom_emoji_document_ids(entities)) > 0;
