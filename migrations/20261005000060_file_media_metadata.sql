-- Existing rows remain documents; this only adds durable metadata columns and
-- extends the accepted subtype-right vocabulary for newly stored photos.
ALTER TABLE files
    ADD COLUMN media_kind TEXT NOT NULL DEFAULT 'document',
    ADD COLUMN width INTEGER NULL,
    ADD COLUMN height INTEGER NULL,
    DROP CONSTRAINT files_subtype_rights_valid,
    ADD CONSTRAINT files_subtype_rights_valid
        CHECK (
            subtype_rights IS NULL OR (
                (array_ndims(subtype_rights) IS NULL OR array_ndims(subtype_rights) = 1)
                AND array_position(subtype_rights, NULL::TEXT) IS NULL
                AND subtype_rights <@ ARRAY[
                    'send_stickers',
                    'send_gifs',
                    'send_videos',
                    'send_roundvideos',
                    'send_audios',
                    'send_voices',
                    'send_photos'
                ]::TEXT[]
            )
        ) NOT VALID,
    ADD CONSTRAINT files_media_metadata_valid
        CHECK (
            (
                media_kind = 'document'
                AND width IS NULL
                AND height IS NULL
                AND (subtype_rights IS NULL OR NOT (subtype_rights @> ARRAY['send_photos']::TEXT[]))
            ) OR (
                media_kind = 'photo'
                AND stored = true
                AND width IS NOT NULL
                AND height IS NOT NULL
                AND width BETWEEN 1 AND 10000
                AND height BETWEEN 1 AND 10000
                AND width::BIGINT + height::BIGINT <= 10000
                AND GREATEST(width, height)::BIGINT <= LEAST(width, height)::BIGINT * 20
                AND width::BIGINT * height::BIGINT <= 16777216
                AND subtype_rights IS NOT NULL
                AND subtype_rights = ARRAY['send_photos']::TEXT[]
            )
        ) NOT VALID;
