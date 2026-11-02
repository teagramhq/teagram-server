-- Support custom-emoji document-reference lookups in private message history.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY messages_custom_emoji_document_ids_gin_idx
    ON messages USING GIN (message_custom_emoji_document_ids(entities))
    WHERE deleted = false AND entities IS NOT NULL;
