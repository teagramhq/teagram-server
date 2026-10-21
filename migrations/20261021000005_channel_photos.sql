-- A channel's selected avatar is owned by the account whose validated upload
-- produced it. The nullable pair represents an empty selection. RESTRICT keeps
-- the original and its derivatives alive while selected; clearing or replacing
-- the pointer is reference removal, not physical erasure.
--
-- Reversible with data loss: dropping these columns after a channel photo is
-- selected loses that current selection. Roll back only before selection data
-- has been written.

ALTER TABLE channels
    ADD COLUMN current_photo_file_id BIGINT NULL,
    ADD COLUMN current_photo_uploader_id BIGINT NULL,
    ADD CONSTRAINT channels_current_photo_fields_paired CHECK (
        (current_photo_file_id IS NULL) = (current_photo_uploader_id IS NULL)
    ),
    ADD CONSTRAINT channels_current_photo_file_owned_by_uploader
        FOREIGN KEY (current_photo_file_id, current_photo_uploader_id)
        REFERENCES files (id, uploader_id)
        ON DELETE RESTRICT;

CREATE INDEX channels_current_photo_file_id_idx
    ON channels (current_photo_file_id)
    WHERE current_photo_file_id IS NOT NULL;
