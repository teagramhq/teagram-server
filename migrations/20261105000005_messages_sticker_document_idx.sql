-- File download checks message sticker references by owner and document.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY messages_owner_sticker_document_idx
    ON messages (owner_id, sticker_document_id)
    WHERE sticker_document_id IS NOT NULL AND deleted = false;
