-- A sentinel row keeps the owner's pull-recovery marker after the final pin is removed.
CREATE TABLE user_dialog_pins (
    owner_id   BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    peer_type  SMALLINT NOT NULL,
    peer_id    BIGINT NOT NULL,
    position   SMALLINT NULL,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (owner_id, peer_type, peer_id),
    CONSTRAINT user_dialog_pins_peer_position CHECK (
        (peer_type = 0 AND peer_id = 0 AND position IS NULL)
        OR (peer_type BETWEEN 1 AND 3 AND peer_id > 0 AND position IS NOT NULL AND position BETWEEN 0 AND 4)
    ),
    CONSTRAINT user_dialog_pins_position_unique UNIQUE (owner_id, position)
);
