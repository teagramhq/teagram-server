-- Last-seen privacy is additive foundation storage. Missing rules retain the
-- existing everybody-visible behavior; no rows are backfilled.
CREATE TABLE user_privacy_rules (
    owner_id    BIGINT NOT NULL REFERENCES users(id),
    privacy_key TEXT   NOT NULL CHECK (privacy_key = 'status_timestamp'),
    base_rule   TEXT   NOT NULL CHECK (base_rule IN ('everybody', 'contacts', 'nobody')),
    PRIMARY KEY (owner_id, privacy_key)
);

CREATE TABLE user_privacy_rule_users (
    owner_id    BIGINT NOT NULL,
    privacy_key TEXT   NOT NULL,
    user_id     BIGINT NOT NULL REFERENCES users(id),
    entry_type  TEXT   NOT NULL CHECK (entry_type IN ('allow', 'disallow')),
    PRIMARY KEY (owner_id, privacy_key, user_id, entry_type),
    FOREIGN KEY (owner_id, privacy_key)
        REFERENCES user_privacy_rules (owner_id, privacy_key) ON DELETE CASCADE,
    CHECK (owner_id <> user_id)
);
