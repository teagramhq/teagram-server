-- Empty caller-scoped state records only add-contact/block bar dismissals.
-- Older releases ignore this additive table; existing users are not backfilled.
-- Retain each preference only while both accounts exist.
CREATE TABLE peer_settings_bar_dismissals (
    owner_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    peer_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (owner_id, peer_id),
    CHECK (owner_id <> peer_id)
);
