ALTER TABLE user_created_sticker_set_items
    ADD COLUMN content_sha256 BYTEA
        CONSTRAINT user_created_sticker_set_items_content_sha256_length
        CHECK (content_sha256 IS NULL OR octet_length(content_sha256) = 32);

CREATE VIEW live_user_created_sticker_sets AS
SELECT id, access_hash, creator_id, title, short_name, software, kind,
       text_color, item_count, set_hash, is_live, created_at
FROM user_created_sticker_sets
WHERE is_live = true;

CREATE TABLE admin_sticker_set_events (
    id                   BIGSERIAL PRIMARY KEY,
    action               TEXT NOT NULL CHECK (action IN ('takedown', 'restore')),
    set_id               BIGINT NOT NULL,
    set_title            TEXT NOT NULL,
    set_short_name       TEXT NOT NULL,
    reason               TEXT NOT NULL CHECK (octet_length(reason) BETWEEN 1 AND 512),
    session_fingerprint  BYTEA NOT NULL CHECK (octet_length(session_fingerprint) = 32),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX admin_sticker_set_events_created_at_idx
    ON admin_sticker_set_events (created_at DESC, id DESC);

CREATE FUNCTION reject_admin_sticker_set_event_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'admin sticker-set audit records are append-only';
END;
$$;

CREATE TRIGGER admin_sticker_set_events_no_update_or_delete
    BEFORE UPDATE OR DELETE ON admin_sticker_set_events
    FOR EACH ROW EXECUTE FUNCTION reject_admin_sticker_set_event_mutation();

CREATE TRIGGER admin_sticker_set_events_no_truncate
    BEFORE TRUNCATE ON admin_sticker_set_events
    FOR EACH STATEMENT EXECUTE FUNCTION reject_admin_sticker_set_event_mutation();
