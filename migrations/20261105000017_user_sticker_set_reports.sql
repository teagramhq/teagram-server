CREATE TABLE user_sticker_set_reports (
    id                BIGSERIAL PRIMARY KEY CHECK (id > 0),
    reporter_id       BIGINT NOT NULL REFERENCES users (id),
    set_id            BIGINT NOT NULL REFERENCES user_created_sticker_sets (id),
    message_peer_type SMALLINT NOT NULL CHECK (message_peer_type IN (1, 2, 3)),
    message_peer_id   BIGINT NOT NULL CHECK (message_peer_id > 0),
    message_id        BIGINT NOT NULL CHECK (message_id > 0),
    reason            TEXT NOT NULL CHECK (reason IN ('copyright', 'abuse', 'other')),
    comment           TEXT NOT NULL DEFAULT '' CHECK (octet_length(comment) <= 512),
    status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'dismissed', 'actioned')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    closed_at         TIMESTAMPTZ,
    CONSTRAINT user_sticker_set_reports_reporter_set_key UNIQUE (reporter_id, set_id),
    CONSTRAINT user_sticker_set_reports_closed_at_check CHECK ((status = 'open') = (closed_at IS NULL))
);

CREATE INDEX user_sticker_set_reports_open_set_created_idx
    ON user_sticker_set_reports (set_id, created_at, id)
    WHERE status = 'open';

CREATE INDEX user_sticker_set_reports_reporter_created_idx
    ON user_sticker_set_reports (reporter_id, created_at);

CREATE INDEX user_sticker_set_reports_closed_at_idx
    ON user_sticker_set_reports (closed_at, id)
    WHERE status <> 'open';
