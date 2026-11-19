ALTER TABLE files
    ADD COLUMN content_sha256 BYTEA;

ALTER TABLE files
    ADD CONSTRAINT files_content_sha256_length
        CHECK (content_sha256 IS NULL OR octet_length(content_sha256) = 32)
        NOT VALID;
