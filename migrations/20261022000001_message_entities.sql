-- Store client-supplied and server-detected entities on new message rows.
-- NULL preserves the behavior of messages written before this migration.
ALTER TABLE messages
    ADD COLUMN entities JSONB;

ALTER TABLE channel_messages
    ADD COLUMN entities JSONB;
