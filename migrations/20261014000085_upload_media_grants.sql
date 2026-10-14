-- An uploadMedia result is not a download capability. This row is an inert,
-- caller-owned capability for the later album-send path; live message rows
-- remain the only download authorization source.
CREATE TABLE file_upload_grants (
    file_id    BIGINT PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    owner_id   BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX file_upload_grants_owner_created_idx
    ON file_upload_grants (owner_id, created_at);
