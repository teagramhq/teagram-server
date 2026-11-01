ALTER TABLE user_created_sticker_set_items
    ADD COLUMN is_active BOOL NOT NULL DEFAULT true;

ALTER TABLE user_created_sticker_sets
    ADD COLUMN thumb_file_id BIGINT,
    ADD COLUMN thumb_format TEXT,
    ADD COLUMN thumb_size BIGINT,
    ADD COLUMN thumb_width INT,
    ADD COLUMN thumb_height INT,
    ADD COLUMN thumb_duration_ns BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN thumb_version INT NOT NULL DEFAULT 0,
    ADD COLUMN thumb_document_id BIGINT NOT NULL DEFAULT 0,
    ADD CONSTRAINT user_created_sticker_sets_thumb_fields_check CHECK (
        (
            thumb_file_id IS NULL
            AND thumb_format IS NULL
            AND thumb_size IS NULL
            AND thumb_width IS NULL
            AND thumb_height IS NULL
            AND thumb_duration_ns = 0
            AND thumb_version >= 0
        )
        OR
        (
            thumb_file_id IS NOT NULL
            AND thumb_format IS NOT NULL
            AND thumb_size IS NOT NULL
            AND thumb_width IS NOT NULL
            AND thumb_height IS NOT NULL
            AND thumb_format IN ('WEBP', 'TGS', 'WEBM')
            AND thumb_size > 0
            AND thumb_width = 100
            AND thumb_height = 100
            AND thumb_duration_ns >= 0
            AND thumb_version >= 0
            AND (
                (thumb_format = 'WEBP' AND thumb_size <= 131072)
                OR (thumb_format IN ('TGS', 'WEBM') AND thumb_size <= 32768)
            )
            AND thumb_document_id = 0
            AND kind <> 'emojis'
        )
    ) NOT VALID,
    ADD CONSTRAINT user_created_sticker_sets_thumb_document_kind_check CHECK (
        thumb_document_id >= 0 AND (thumb_document_id = 0 OR kind = 'emojis')
    ) NOT VALID,
    ADD CONSTRAINT user_created_sticker_sets_thumb_version_check CHECK (
        thumb_version >= 0
    ) NOT VALID;

ALTER TABLE user_created_sticker_sets
    ADD CONSTRAINT user_created_sticker_sets_thumb_file_id_fkey
    FOREIGN KEY (thumb_file_id) REFERENCES files (id) ON DELETE RESTRICT NOT VALID;
