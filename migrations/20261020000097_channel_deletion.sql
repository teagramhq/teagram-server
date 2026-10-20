ALTER TABLE channels
    ADD COLUMN deleted_at TIMESTAMPTZ NULL;

ALTER TABLE usernames
    DROP CONSTRAINT usernames_owner_type_check,
    ADD CONSTRAINT usernames_owner_type_check
        CHECK (owner_type IN ('user', 'channel', 'channel_hold')),
    ADD COLUMN hold_creator_id BIGINT NULL,
    ADD COLUMN hold_until TIMESTAMPTZ NULL,
    ADD CONSTRAINT usernames_channel_hold_fields_check CHECK (
        (owner_type = 'channel_hold' AND hold_creator_id IS NOT NULL AND hold_until IS NOT NULL)
        OR (owner_type <> 'channel_hold' AND hold_creator_id IS NULL AND hold_until IS NULL)
    );

CREATE TABLE channel_deletion_jobs (
    channel_id             BIGINT PRIMARY KEY REFERENCES channels (id),
    post_cursor            BIGINT NOT NULL DEFAULT 0 CHECK (post_cursor >= 0),
    posts_tombstoned_at    TIMESTAMPTZ NULL
);
