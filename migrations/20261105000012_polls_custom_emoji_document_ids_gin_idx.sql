-- Index canonical poll descriptions by the requested custom-emoji document IDs.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY polls_custom_emoji_document_ids_gin_idx
    ON polls USING GIN (message_custom_emoji_document_ids(description_entities))
    WHERE cardinality(message_custom_emoji_document_ids(description_entities)) > 0;
