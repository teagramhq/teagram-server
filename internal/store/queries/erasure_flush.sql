-- Inert flush persistence over the erasure outbox. These generated access points
-- have no runtime caller; TestErasureLedgerPersistenceHasNoRuntimeCaller enforces
-- that boundary. No query here updates or deletes a confirmation or a closed
-- marker, deletes a claim, or lowers a claim generation.
--
-- A position is flushable when its stream is open, its outbox row exists, it has
-- no confirmation, and it is seq 1 or seq - 1 is confirmed. "First" is seq 1 and
-- never "no lower outbox row": the predecessor's outbox row may already be gone.

-- GrantErasureFlushClaim creates the stream's claim at generation 1. It returns
-- no row when the claim exists or the position is not flushable.
-- name: GrantErasureFlushClaim :one
INSERT INTO erasure_flush_claim (epoch, stream_id, position, holder, generation)
SELECT o.epoch, o.stream_id, o.seq, sqlc.arg(holder), 1
FROM erasure_outbox AS o
WHERE o.epoch = sqlc.arg(epoch)
  AND o.stream_id = sqlc.arg(stream_id)
  AND o.seq = sqlc.arg(position)
  AND NOT EXISTS (
      SELECT FROM erasure_flush_confirmation AS c
      WHERE c.epoch = o.epoch AND c.stream_id = o.stream_id AND c.seq = o.seq
  )
  AND (o.seq = 1 OR EXISTS (
      SELECT FROM erasure_flush_confirmation AS p
      WHERE p.epoch = o.epoch AND p.stream_id = o.stream_id AND p.seq = o.seq - 1
  ))
  AND NOT EXISTS (
      SELECT FROM erasure_flush_closed_stream AS x
      WHERE x.epoch = o.epoch AND x.stream_id = o.stream_id
  )
ON CONFLICT (epoch, stream_id) DO NOTHING
RETURNING generation;

-- AdvanceErasureFlushClaim moves the holder's claim to a flushable position and
-- raises its generation. It returns no row unless the caller holds the claim at
-- the generation it read.
-- name: AdvanceErasureFlushClaim :one
UPDATE erasure_flush_claim AS claim
SET position = o.seq, generation = claim.generation + 1
FROM erasure_outbox AS o
WHERE claim.epoch = sqlc.arg(epoch)
  AND claim.stream_id = sqlc.arg(stream_id)
  AND claim.holder = sqlc.arg(holder)
  AND claim.generation = sqlc.arg(generation)
  AND claim.generation < 9223372036854775807
  AND o.epoch = claim.epoch
  AND o.stream_id = claim.stream_id
  AND o.seq = sqlc.arg(position)
  AND NOT EXISTS (
      SELECT FROM erasure_flush_confirmation AS c
      WHERE c.epoch = o.epoch AND c.stream_id = o.stream_id AND c.seq = o.seq
  )
  AND (o.seq = 1 OR EXISTS (
      SELECT FROM erasure_flush_confirmation AS p
      WHERE p.epoch = o.epoch AND p.stream_id = o.stream_id AND p.seq = o.seq - 1
  ))
  AND NOT EXISTS (
      SELECT FROM erasure_flush_closed_stream AS x
      WHERE x.epoch = o.epoch AND x.stream_id = o.stream_id
  )
RETURNING claim.generation;

-- TakeOverErasureFlushClaim hands the claim to a new holder at the same position
-- and raises its generation, so the previous holder's confirmation refuses.
-- name: TakeOverErasureFlushClaim :one
UPDATE erasure_flush_claim AS claim
SET holder = sqlc.arg(holder), generation = claim.generation + 1
WHERE claim.epoch = sqlc.arg(epoch)
  AND claim.stream_id = sqlc.arg(stream_id)
  AND claim.generation = sqlc.arg(generation)
  AND claim.generation < 9223372036854775807
  AND NOT EXISTS (
      SELECT FROM erasure_flush_closed_stream AS x
      WHERE x.epoch = claim.epoch AND x.stream_id = claim.stream_id
  )
RETURNING claim.position, claim.generation;

-- ConfirmErasureFlushPosition is the one pending-to-confirmed transition. The
-- claim row is locked first, and its generation is part of the locking
-- predicate, so a takeover committed while this statement waits leaves no claim
-- row and no insert. The checksum is computed here over the current outbox
-- bytes; a value the caller computed earlier does not count. An existing
-- confirmation is never changed: the conflict affects zero rows.
-- name: ConfirmErasureFlushPosition :execrows
WITH claim AS (
    SELECT c.epoch, c.stream_id, c.position
    FROM erasure_flush_claim AS c
    WHERE c.epoch = sqlc.arg(epoch)
      AND c.stream_id = sqlc.arg(stream_id)
      AND c.position = sqlc.arg(seq)
      AND c.generation = sqlc.arg(generation)
    FOR UPDATE
)
INSERT INTO erasure_flush_confirmation (
    epoch, stream_id, seq, operation_key, checksum, arrival_seq, proof_kind
)
SELECT o.epoch, o.stream_id, o.seq, o.operation_key,
       sqlc.arg(checksum)::bytea, sqlc.arg(arrival_seq)::bigint, sqlc.arg(proof_kind)::text
FROM claim
INNER JOIN erasure_outbox AS o
    ON o.epoch = claim.epoch AND o.stream_id = claim.stream_id AND o.seq = claim.position
WHERE o.operation_key = sqlc.arg(operation_key)
  AND sha256(o.record) = sqlc.arg(checksum)::bytea
  AND (o.seq = 1 OR EXISTS (
      SELECT FROM erasure_flush_confirmation AS p
      WHERE p.epoch = o.epoch AND p.stream_id = o.stream_id AND p.seq = o.seq - 1
  ))
  AND NOT EXISTS (
      SELECT FROM erasure_flush_closed_stream AS x
      WHERE x.epoch = o.epoch AND x.stream_id = o.stream_id
  )
ON CONFLICT DO NOTHING;

-- CloseErasureFlushStream records a lost-lineage collision. A repeated close
-- keeps the first marker.
-- name: CloseErasureFlushStream :execrows
INSERT INTO erasure_flush_closed_stream (epoch, stream_id, position)
VALUES (sqlc.arg(epoch), sqlc.arg(stream_id), sqlc.arg(position))
ON CONFLICT (epoch, stream_id) DO NOTHING;

-- name: ErasureFlushClaimByStream :one
SELECT * FROM erasure_flush_claim
WHERE epoch = $1 AND stream_id = $2;

-- name: ErasureFlushStreamConfirmations :many
SELECT * FROM erasure_flush_confirmation
WHERE epoch = $1 AND stream_id = $2
ORDER BY seq;

-- name: ErasureFlushClosedStreamByStream :one
SELECT * FROM erasure_flush_closed_stream
WHERE epoch = $1 AND stream_id = $2;
