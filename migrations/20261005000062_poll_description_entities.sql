-- Poll descriptions are message captions; retain their formatting entities
-- with the canonical poll so retries and every message read path agree.
ALTER TABLE polls
    ADD COLUMN description_entities BYTEA NOT NULL DEFAULT ''::bytea;
