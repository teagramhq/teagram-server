CREATE TABLE notify_settings_difference_cursors (
    auth_key_id      BIGINT PRIMARY KEY REFERENCES auth_keys (id) ON DELETE CASCADE,
    user_id          BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    cutoff           TIMESTAMPTZ NOT NULL,
    resume_date      BIGINT NOT NULL,
    last_changed_at  TIMESTAMPTZ NOT NULL,
    last_scope       SMALLINT NOT NULL,
    last_peer_type   SMALLINT NOT NULL,
    last_peer_id     BIGINT NOT NULL,
    state_pts        BIGINT NOT NULL,
    state_qts        BIGINT NOT NULL,
    state_date       BIGINT NOT NULL,
    state_seq        BIGINT NOT NULL
);
