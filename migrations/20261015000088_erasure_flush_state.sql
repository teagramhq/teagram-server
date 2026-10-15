-- Inert flush state for the erasure outbox: the per-stream claim, the per-position
-- confirmation and the closed-stream marker. No live path reads or writes these
-- tables; their only callers are tests, and the outbox and capture surface are
-- unchanged.
-- Additive and reversible while inert: dropping the three tables restores the
-- previous schema with no data loss.

-- One claim per (epoch, stream), naming the single position that may be flushed.
-- Grant, advance and takeover are compare-and-swap writes that only ever raise
-- generation, so a stale holder can never get its old generation back. The row is
-- never deleted for the same reason. There is no clock column: lease expiry
-- belongs to a later runtime slice.
CREATE TABLE erasure_flush_claim (
    epoch      BIGINT NOT NULL CHECK (epoch >= 1),
    stream_id  BYTEA  NOT NULL CHECK (octet_length(stream_id) = 16),
    position   BIGINT NOT NULL CHECK (position >= 1),
    holder     BYTEA  NOT NULL CHECK (octet_length(holder) = 16),
    generation BIGINT NOT NULL CHECK (generation >= 1),
    PRIMARY KEY (epoch, stream_id)
);

-- Confirmation evidence lives outside the immutable outbox and carries no
-- foreign key to it, so a confirmed predecessor stays provable after its outbox
-- row is removed locally. checksum is the SHA-256 of the outbox record that the
-- confirming statement computed and compared; proof_kind admits only synthetic
-- proof until a real provider is qualified. Rows are only ever inserted.
CREATE TABLE erasure_flush_confirmation (
    epoch         BIGINT NOT NULL CHECK (epoch >= 1),
    stream_id     BYTEA  NOT NULL CHECK (octet_length(stream_id) = 16),
    seq           BIGINT NOT NULL CHECK (seq >= 1),
    operation_key BYTEA  NOT NULL CHECK (octet_length(operation_key) = 16),
    checksum      BYTEA  NOT NULL CHECK (octet_length(checksum) = 32),
    arrival_seq   BIGINT NOT NULL CHECK (arrival_seq >= 1),
    proof_kind    TEXT   NOT NULL CHECK (proof_kind = 'synthetic'),
    PRIMARY KEY (epoch, stream_id, seq),
    CONSTRAINT erasure_flush_confirmation_operation_key_unique
        UNIQUE (operation_key)
);

-- A stream closed by a lost-lineage collision at position. It is never reopened
-- in this slice: claims and confirmations on it refuse, other streams progress.
CREATE TABLE erasure_flush_closed_stream (
    epoch     BIGINT NOT NULL CHECK (epoch >= 1),
    stream_id BYTEA  NOT NULL CHECK (octet_length(stream_id) = 16),
    position  BIGINT NOT NULL CHECK (position >= 1),
    PRIMARY KEY (epoch, stream_id)
);
