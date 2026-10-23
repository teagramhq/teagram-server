-- Catalogue documents have their own identity and download lane. Keep those
-- identities out of files and outside the file-reference foreign keys.
ALTER TABLE messages
    ADD COLUMN sticker_document_id BIGINT NULL,
    ADD CONSTRAINT messages_sticker_document_separate_check
        CHECK (sticker_document_id IS NULL OR (sticker_document_id < 0 AND file_id = 0));

ALTER TABLE channel_messages
    ADD COLUMN sticker_document_id BIGINT NULL,
    ADD CONSTRAINT channel_messages_sticker_document_separate_check
        CHECK (sticker_document_id IS NULL OR (sticker_document_id < 0 AND file_id IS NULL));
