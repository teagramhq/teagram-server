ALTER TABLE messages
    ADD COLUMN reply_to_trusted BOOLEAN NOT NULL DEFAULT false;
