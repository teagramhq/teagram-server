-- File download checks channel sticker references before resolving membership.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY channel_messages_sticker_document_channel_idx
    ON channel_messages (sticker_document_id, channel_id, local_id)
    WHERE sticker_document_id IS NOT NULL AND deleted = false;
