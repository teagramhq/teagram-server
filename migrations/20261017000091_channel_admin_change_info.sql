-- Retain the channel admin right that gates channel metadata edits. The
-- default keeps existing participants and older binaries compatible.
ALTER TABLE channel_participants
    ADD COLUMN admin_change_info BOOL NOT NULL DEFAULT false;
