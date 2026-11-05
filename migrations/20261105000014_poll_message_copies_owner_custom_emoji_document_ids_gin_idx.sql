-- Scope private poll custom-emoji lookups by owner and requested document IDs.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY poll_message_copies_owner_custom_emoji_document_ids_gin_idx
    ON poll_message_copies USING GIN (owner_id, custom_emoji_document_ids)
    WHERE cardinality(custom_emoji_document_ids) > 0;
