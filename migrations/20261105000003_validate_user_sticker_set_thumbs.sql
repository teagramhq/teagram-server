ALTER TABLE user_created_sticker_sets
    VALIDATE CONSTRAINT user_created_sticker_sets_thumb_fields_check;

ALTER TABLE user_created_sticker_sets
    VALIDATE CONSTRAINT user_created_sticker_sets_thumb_document_kind_check;

ALTER TABLE user_created_sticker_sets
    VALIDATE CONSTRAINT user_created_sticker_sets_thumb_version_check;

ALTER TABLE user_created_sticker_sets
    VALIDATE CONSTRAINT user_created_sticker_sets_thumb_file_id_fkey;
