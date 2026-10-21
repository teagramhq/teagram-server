-- Add the durable current-photo pointer for basic groups. The photo is owned by
-- the member whose validated upload produced it; the composite foreign key
-- makes that attribution structural and RESTRICT keeps the original and its
-- derivatives alive while the group selects it. The nullable pair is the empty
-- selection. There is no backfill because basic groups have no stored photo
-- state before this migration.
--
-- Reversible with data loss: dropping these columns after a writer has stored a
-- selection loses that current selection. The application can roll back safely
-- only before any basic-group photo is selected.

ALTER TABLE chats
    ADD COLUMN current_photo_file_id BIGINT NULL,
    ADD COLUMN current_photo_uploader_id BIGINT NULL,
    ADD CONSTRAINT chats_current_photo_fields_paired CHECK (
        (current_photo_file_id IS NULL) = (current_photo_uploader_id IS NULL)
    ),
    ADD CONSTRAINT chats_current_photo_file_owned_by_uploader
        FOREIGN KEY (current_photo_file_id, current_photo_uploader_id)
        REFERENCES files (id, uploader_id)
        ON DELETE RESTRICT;

-- The eraser asks whether any chat selects a file. This reverse index keeps
-- that per-file probe bounded as the number of groups grows.
CREATE INDEX chats_current_photo_file_id_idx
    ON chats (current_photo_file_id)
    WHERE current_photo_file_id IS NOT NULL;
