-- Fleet telemetry is process-generation scoped and disposable. It contains
-- only live gauges and an ephemeral set of connected account identifiers.
CREATE UNLOGGED TABLE fleet_process_snapshots (
    generation TEXT PRIMARY KEY
        CHECK (generation ~ '^[0-9a-f]{32}$'),
    replica_id TEXT
        CHECK (replica_id IS NULL OR replica_id ~ '^[A-Za-z0-9._-]{1,64}$'),
    version TEXT NOT NULL DEFAULT ''
        CHECK (length(version) <= 128 AND version ~ '^[A-Za-z0-9._+-]*$'),
    process_started_at TIMESTAMPTZ NOT NULL,
    heartbeat_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    connections BIGINT NOT NULL CHECK (connections >= 0),
    sessions BIGINT NOT NULL CHECK (sessions >= 0),
    accounts_complete BOOLEAN NOT NULL,
    CHECK (process_started_at <= heartbeat_at),
    CHECK (heartbeat_at < expires_at),
    CHECK (accounts_complete = (sessions <= 100000))
);

CREATE INDEX fleet_process_snapshots_expiry_idx
    ON fleet_process_snapshots (expires_at, generation);

CREATE INDEX fleet_process_snapshots_replica_started_idx
    ON fleet_process_snapshots (replica_id, process_started_at DESC)
    INCLUDE (expires_at)
    WHERE replica_id IS NOT NULL;

CREATE UNLOGGED TABLE fleet_live_accounts (
    generation TEXT NOT NULL
        CHECK (generation ~ '^[0-9a-f]{32}$'),
    user_id BIGINT NOT NULL CHECK (user_id > 0),
    PRIMARY KEY (generation, user_id)
);
