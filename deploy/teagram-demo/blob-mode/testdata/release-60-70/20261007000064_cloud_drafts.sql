CREATE TABLE cloud_drafts (
    owner_id         BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    peer_type        SMALLINT NOT NULL,
    peer_id          BIGINT NOT NULL,
    message           TEXT NOT NULL,
    no_webpage        BOOL NOT NULL DEFAULT false,
    reply_to_msg_id   BIGINT NULL,
    updated_at        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, peer_type, peer_id),
    CONSTRAINT cloud_drafts_peer CHECK (peer_type BETWEEN 1 AND 3 AND peer_id > 0),
    CONSTRAINT cloud_drafts_reply CHECK (reply_to_msg_id IS NULL OR reply_to_msg_id > 0)
);

CREATE TABLE cloud_draft_sync (
    owner_id   BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    peer_type  SMALLINT NOT NULL,
    peer_id    BIGINT NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_id, peer_type, peer_id),
    CONSTRAINT cloud_draft_sync_peer CHECK (peer_type BETWEEN 1 AND 3 AND peer_id > 0)
);

CREATE INDEX cloud_draft_sync_changed_idx
    ON cloud_draft_sync (owner_id, changed_at, peer_type, peer_id);
