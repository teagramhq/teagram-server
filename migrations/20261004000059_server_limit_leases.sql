-- Ephemeral leases for cluster-wide concurrent server limits.
--
-- A lease is one admitted operation or connection. The owner releases it on
-- exit; expires_at reclaims it after a process crash or lost cleanup.
CREATE TABLE server_limit_leases (
    lease_id   BYTEA       NOT NULL PRIMARY KEY CHECK (octet_length(lease_id) = 16),
    subject_id BIGINT      NOT NULL,
    surface    TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX server_limit_leases_subject_surface_expiry_idx
    ON server_limit_leases (subject_id, surface, expires_at);
CREATE INDEX server_limit_leases_expires_at_idx
    ON server_limit_leases (expires_at);
