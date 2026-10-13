-- Alpha-local request receipts and capacity-refusal totals. No live caller
-- reads or writes either table. Transport identity, targets and caller results
-- stay here, outside immutable erasure-ledger bodies.
-- Additive and reversible while inert: the old application ignores this schema.

CREATE FUNCTION message_delete_receipt_targets_are_normalized(targets INTEGER[])
RETURNS BOOLEAN
LANGUAGE SQL IMMUTABLE STRICT
AS $$
    SELECT cardinality(targets) <= 100
       AND NOT EXISTS (SELECT FROM unnest(targets) AS t(id) WHERE id IS NULL OR id <= 0)
       AND targets = ARRAY(SELECT DISTINCT id FROM unnest(targets) AS t(id) ORDER BY id)
$$;

CREATE TABLE message_delete_receipts (
    owner_id           BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    auth_key_id        BYTEA       NOT NULL CHECK (octet_length(auth_key_id) = 8),
    session_id         BIGINT      NOT NULL,
    msg_id             BIGINT      NOT NULL,
    method             TEXT        NOT NULL CHECK (method = 'messages.deleteMessages'),
    id_space           TEXT        NOT NULL CHECK (id_space = 'owner_message'),
    revoke             BOOLEAN     NOT NULL,
    target_ids         INTEGER[]   NOT NULL CHECK (message_delete_receipt_targets_are_normalized(target_ids)),
    pts                INTEGER     NOT NULL CHECK (pts >= 0),
    pts_count          INTEGER     NOT NULL CHECK (pts_count >= 0),
    receipt_key        BYTEA       NOT NULL CHECK (
        octet_length(receipt_key) = 16 AND receipt_key <> decode('00000000000000000000000000000000', 'hex')
    ),
    state              TEXT        NOT NULL,
    operation_key      BYTEA,
    epoch              BIGINT,
    stream_id          BYTEA,
    position           BIGINT,
    confirmed_at       TIMESTAMPTZ,
    local_completed_at TIMESTAMPTZ,
    retain_until       TIMESTAMPTZ,
    PRIMARY KEY (owner_id, auth_key_id, session_id, msg_id),
    CONSTRAINT message_delete_receipts_key_unique UNIQUE (receipt_key),
    CONSTRAINT message_delete_receipts_capture_reference CHECK (
        (operation_key IS NULL AND epoch IS NULL AND stream_id IS NULL AND position IS NULL)
        OR (
            operation_key IS NOT NULL AND octet_length(operation_key) = 16
            AND operation_key <> decode('00000000000000000000000000000000', 'hex')
            AND epoch IS NOT NULL AND epoch >= 1
            AND stream_id IS NOT NULL AND octet_length(stream_id) = 16
            AND stream_id <> decode('00000000000000000000000000000000', 'hex')
            AND position IS NOT NULL AND position >= 1
        )
    ),
    CONSTRAINT message_delete_receipts_state CHECK (
        (state = 'pending' AND operation_key IS NOT NULL AND cardinality(target_ids) > 0
            AND confirmed_at IS NULL AND local_completed_at IS NULL AND retain_until IS NULL)
        OR (state = 'confirmed' AND operation_key IS NOT NULL AND cardinality(target_ids) > 0
            AND confirmed_at IS NOT NULL AND local_completed_at IS NULL AND retain_until IS NOT NULL)
        OR (state = 'local_no_change' AND operation_key IS NULL
            AND confirmed_at IS NULL AND local_completed_at IS NOT NULL AND retain_until IS NOT NULL)
    ),
    -- 97 elapsed days, independent of the connection's timezone and DST.
    -- Pending has no deadline or expiry authority. There is no confirmation or
    -- removal writer; a later conditional transition must set its own anchor.
    CONSTRAINT message_delete_receipts_retention CHECK (
        (confirmed_at IS NULL OR isfinite(confirmed_at))
        AND (local_completed_at IS NULL OR isfinite(local_completed_at))
        AND (retain_until IS NULL OR (
            isfinite(retain_until)
            AND retain_until >= COALESCE(confirmed_at, local_completed_at) + INTERVAL '8380800 seconds'
        ))
    )
);

-- The identity key serves point lookups. A narrow owner index also bounds
-- retained counts without reading another owner's wide target/result rows.
CREATE INDEX message_delete_receipts_owner_idx ON message_delete_receipts (owner_id);

-- No auth_keys FK: logout/deletion/rebinding must preserve the original owner's
-- receipt. The owner FKs cascade so these tables cannot block account deletion.
-- No outbox FK: this opaque reference outlives local outbox removal.
CREATE TABLE message_delete_capacity_refusals (
    owner_id BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    total    BIGINT NOT NULL DEFAULT 0 CHECK (total >= 0)
);
