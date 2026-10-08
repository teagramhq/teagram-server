-- Generated access to the inert erasure-ledger tables. There is no store method
-- over any of these: the persistence slice is inert, so the only callers in the
-- repository are the acceptance tests that prove the storage contract. A runtime
-- writer is a later slice, and its absence is asserted by
-- TestErasureLedgerPersistenceHasNoRuntimeCaller.

-- InsertErasureOutboxRecord writes one record of the outbox. It is an INSERT and
-- not an upsert on purpose: the operation key is the idempotency interlock, so a
-- retried write must be refused by the database rather than replace the committed
-- record it names. The envelope columns are a projection of the canonical record
-- bytes, which a caller produces with the codec.
-- name: InsertErasureOutboxRecord :exec
INSERT INTO erasure_outbox (operation_key, epoch, stream_id, seq, kind, record)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ErasureOutboxByOperationKey :one
SELECT * FROM erasure_outbox WHERE operation_key = $1;

-- ErasureOutboxStreamRecords walks one (epoch, stream) in seq order, the
-- enumeration order the completeness contract is read in: a gap in seq is
-- then visible to the caller rather than absorbed by the walk.
-- name: ErasureOutboxStreamRecords :many
SELECT * FROM erasure_outbox
WHERE epoch = $1 AND stream_id = $2
ORDER BY seq;

-- name: ErasureOutboxTotal :one
SELECT count(*)::bigint AS total FROM erasure_outbox;

-- name: InsertErasureEpochMarker :exec
INSERT INTO erasure_epoch (epoch, lineage_id)
VALUES ($1, $2);

-- InsertErasureEpochCompletion records that one lineage finished replay for one
-- epoch. It is an INSERT because completion is monotone: the row's existence is
-- the fact, and no write here can lower it.
-- name: InsertErasureEpochCompletion :exec
INSERT INTO erasure_epoch_completion (epoch, lineage_id)
VALUES ($1, $2);

-- ErasureEpochLineageState answers the admission question in the only form that
-- is safe to answer: for this epoch AND this lineage, is it complete. A lineage
-- with no completion row is reported not complete even when another lineage
-- shares the epoch number and is complete.
-- name: ErasureEpochLineageState :one
SELECT e.epoch,
       e.lineage_id,
       (c.lineage_id IS NOT NULL)::boolean AS completed
FROM erasure_epoch e
LEFT JOIN erasure_epoch_completion c ON c.epoch = e.epoch AND c.lineage_id = e.lineage_id
WHERE e.epoch = $1 AND e.lineage_id = $2;

-- ErasureEpochLineagesForEpoch lists the lineages that established one epoch,
-- which is how a restore sees that two lineages share an epoch number.
-- name: ErasureEpochLineagesForEpoch :many
SELECT * FROM erasure_epoch
WHERE epoch = $1
ORDER BY lineage_id;

-- name: ErasureEpochMarkerTotal :one
SELECT count(*)::bigint AS total FROM erasure_epoch;

-- name: ErasureEpochCompletionTotal :one
SELECT count(*)::bigint AS total FROM erasure_epoch_completion;
