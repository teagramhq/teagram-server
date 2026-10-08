CREATE TABLE user_dialog_unread_marks (
    owner_id   BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    peer_type  SMALLINT NOT NULL,
    peer_id    BIGINT NOT NULL,
    unread     BOOL NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, peer_type, peer_id),
    CONSTRAINT user_dialog_unread_marks_peer CHECK (peer_type BETWEEN 1 AND 3 AND peer_id > 0)
);

CREATE INDEX user_dialog_unread_marks_changed_idx
    ON user_dialog_unread_marks (owner_id, changed_at, peer_type, peer_id);
