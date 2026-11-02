-- Scope private custom-emoji history lookups by owner and document ID.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY messages_owner_custom_emoji_document_ids_gin_idx
    ON messages USING GIN (owner_id, message_custom_emoji_document_ids(entities))
    WHERE deleted = false AND entities IS NOT NULL;
