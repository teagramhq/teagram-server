CREATE TABLE user_notify_settings (
    owner_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scope         SMALLINT NOT NULL,
    peer_type     SMALLINT NOT NULL DEFAULT 0,
    peer_id       BIGINT NOT NULL DEFAULT 0,
    show_previews BOOLEAN,
    silent        BOOLEAN,
    mute_until    INTEGER,
    sound         SMALLINT,
    changed_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, scope, peer_type, peer_id),
    CONSTRAINT user_notify_settings_scope_key CHECK (
        (scope = 1 AND peer_type BETWEEN 1 AND 3 AND peer_id > 0)
        OR (scope BETWEEN 2 AND 4 AND peer_type = 0 AND peer_id = 0)
    ),
    CONSTRAINT user_notify_settings_sound CHECK (sound IS NULL OR sound IN (0, 1))
);

CREATE INDEX user_notify_settings_changed_idx
    ON user_notify_settings (owner_id, changed_at, scope, peer_type, peer_id);
