-- Inert erasure-ledger persistence: the alpha-local transactional outbox, and
-- the epoch and lineage markers a future restore gate reads. Nothing in internal/
-- or cmd/ writes or reads these tables. No delete, revoke, sweep or eraser path
-- composes a record with its transaction, so every one of them stays empty in
-- production, and the contract below is provable before a writer exists that
-- could get it wrong.
--
-- Reversible: nothing reads these tables, so dropping them restores the previous
-- schema with no data loss.

-- The outbox is pending state, never durable erasure. A row is a ledger record
-- that committed locally and is still waiting to reach the off-alpha
-- destination. Its presence proves nothing about durability, restore readiness,
-- provider capability or erasure completion, and it must never be read as the
-- erasure itself: the message copy is already gone when this row is written, and
-- the record is what lets a restore re-enforce that absence.
--
-- The primary key (epoch, stream_id, seq) is the record's ordering identity,
-- spelled exactly as the codec spells it, and it is the enumeration path: a
-- reader walks one (epoch, stream) in seq order, so a missing seq is a visible
-- gap and not a record that went missing. seq is supplied by the writer and
-- starts at 1. There is no sequence, no column default and no counter on this
-- table on purpose: an allocator would fill the gap the reader is looking for,
-- and the contiguity a caller observes can only be the contiguity the writer
-- chose to keep.
--
-- UNIQUE (operation_key) is the idempotency interlock. A retried write is
-- refused by the database; it is never an update, so a duplicate cannot quietly
-- rewrite the record it names. The key is opaque 128-bit randomness drawn
-- by the caller, never derived from an owner id, a local id, a transport
-- identity or any payload value, which is what makes it safe to key on at all.
--
-- kind is bounded to the accepted vocabulary, 1 through 9. A kind outside that
-- range is refused at the storage boundary, so a kind cannot reach storage ahead
-- of a reader that understands it: widening the accepted set is an explicit
-- migration, the write-side form of the codec's fail-closed read.
--
-- record carries the canonical encoded record, the bytes a replayer
-- consumes. The envelope columns above are a projection of it for indexing, not
-- a second source of truth: the codec's canonical form means a writer that
-- encodes what it stores keeps the two equal, and a decoded stored record is
-- checked against its columns. The length bound is the codec's record bound.
--
-- There is deliberately no created_at, no status, no attempt count and no
-- next_attempt_at. The codec has no timestamp field on purpose: a ledger ordered
-- by a writer's clock is a ledger whose erasure order depends on a replica's
-- wall time. Ordering here is (epoch, stream, seq), and off-alpha it comes from
-- provider arrival evidence. A flush state, a retry clock and a provider
-- outcome belong to the tables that carry them, not to this one.
CREATE TABLE erasure_outbox (
    operation_key BYTEA       NOT NULL CHECK (octet_length(operation_key) = 16),
    epoch         BIGINT      NOT NULL CHECK (epoch >= 1),
    stream_id     BYTEA       NOT NULL CHECK (octet_length(stream_id) = 16),
    seq           BIGINT      NOT NULL CHECK (seq >= 1),
    kind          SMALLINT    NOT NULL CHECK (kind BETWEEN 1 AND 9),
    record        BYTEA       NOT NULL CHECK (octet_length(record) BETWEEN 1 AND 65536),
    PRIMARY KEY (epoch, stream_id, seq),
    CONSTRAINT erasure_outbox_operation_key_unique
        UNIQUE (operation_key)
);

-- erasure_outbox.epoch carries no foreign key to erasure_epoch, and that is a
-- decision, not an omission. A record is written under the epoch its writer is
-- running in, and the marker for that epoch is itself a ledger record that can
-- legitimately land after the records it fences. An FK here would refuse
-- pending state that is legal, and would make the outbox depend on replay
-- progress — the dependency the outbox exists to avoid.

-- One established epoch for one lineage. The pair (epoch, lineage_id) is the key
-- because a completed epoch is a fact about a lineage, not about an epoch
-- number. A restore of an older lineage shares epoch numbers with the lineage it
-- replaced, and completion is what lets a later restore skip records that
-- lineage already applied. Keyed on epoch alone, lineage A's completion would be
-- readable as evidence for the restored lineage B of the same number, which is
-- precisely the mistake the completed-lineage rule exists to prevent.
--
-- lineage_id is opaque 128-bit randomness, so this table carries no
-- name, no host and no restore topology.
CREATE TABLE erasure_epoch (
    epoch      BIGINT NOT NULL CHECK (epoch >= 1),
    lineage_id BYTEA  NOT NULL CHECK (octet_length(lineage_id) = 16),
    PRIMARY KEY (epoch, lineage_id)
);

-- Completion of one (epoch, lineage), kept as a row of its own rather than as a
-- level column on the table above. The order is established-then-completed and
-- it is monotone: nothing may mean "completed, then not
-- completed". A level column can be rewritten back to a lower value by any
-- UPDATE; the existence of this row cannot, and the foreign key below restricts
-- removal of the marker it completes.
--
-- The row carries no column beyond the key. The accepted epoch body names a
-- number, a lineage and a level, so there is no timestamp, owner, progress
-- counter or margin to store, and inventing one here would be an obligation no
-- writer can satisfy.
CREATE TABLE erasure_epoch_completion (
    epoch      BIGINT NOT NULL CHECK (epoch >= 1),
    lineage_id BYTEA  NOT NULL CHECK (octet_length(lineage_id) = 16),
    PRIMARY KEY (epoch, lineage_id),
    CONSTRAINT erasure_epoch_completion_marker_exists
        FOREIGN KEY (epoch, lineage_id) REFERENCES erasure_epoch (epoch, lineage_id)
        ON DELETE RESTRICT
);
