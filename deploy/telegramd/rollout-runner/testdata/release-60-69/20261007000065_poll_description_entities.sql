-- Store the server's stable, typed entity allowlist rather than gotd TL bytes;
-- poll descriptions are message captions shared by retries and every read path.
ALTER TABLE polls
    ADD COLUMN description_entities JSONB NOT NULL DEFAULT '{"version":1,"entities":[]}'::jsonb;
