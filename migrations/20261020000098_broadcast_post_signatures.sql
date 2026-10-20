-- Store broadcast signature settings and their immutable send-time snapshots.
-- Forwarded post authors are copied into the forward header snapshot so a
-- later toggle, rename, or source-post tombstone cannot change an existing forward.
ALTER TABLE channels
    ADD COLUMN signatures BOOL NOT NULL DEFAULT false;

ALTER TABLE channel_messages
    ADD COLUMN post_author TEXT NULL;

ALTER TABLE messages
    ADD COLUMN fwd_post_author TEXT NULL;
