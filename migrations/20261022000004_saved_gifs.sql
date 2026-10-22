CREATE TABLE saved_gifs (
    owner_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id  BIGINT NOT NULL,
    saved_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, file_id)
);

CREATE INDEX saved_gifs_owner_saved_at_idx
    ON saved_gifs (owner_id, saved_at DESC, file_id DESC);
