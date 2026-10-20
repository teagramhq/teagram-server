-- Keep hidden channel history opt-in and existing members' history unchanged.
ALTER TABLE channels
    ADD COLUMN pre_history_hidden BOOL NOT NULL DEFAULT false;

ALTER TABLE channel_participants
    ADD COLUMN available_min_id BIGINT NOT NULL DEFAULT 0;
