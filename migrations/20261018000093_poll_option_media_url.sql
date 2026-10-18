ALTER TABLE poll_options
    ADD COLUMN media_url TEXT,
    ADD CONSTRAINT poll_options_media_url_length_check
        CHECK (media_url IS NULL OR octet_length(media_url) BETWEEN 1 AND 2048);
