-- Inert persistence only. These generated access points have no runtime caller;
-- TestErasureLedgerPersistenceHasNoRuntimeCaller enforces that boundary.
-- A receipt result is locally committed state, never a durable acknowledgement.

-- Identity collisions fail rather than overwrite the first result.
-- name: InsertMessageDeletePendingReceipt :one
INSERT INTO message_delete_receipts (
    owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
    pts, pts_count, receipt_key, state, operation_key, epoch, stream_id, position
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'pending', $12, $13, $14, $15)
RETURNING *;

-- Local completion is assigned once by the database in the inserting statement.
-- A supplied later inventory deadline is retained; NULL uses the 97-day floor.
-- name: InsertMessageDeleteNoChangeReceipt :one
WITH completion AS (SELECT statement_timestamp() AS at)
INSERT INTO message_delete_receipts (
    owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
    pts, pts_count, receipt_key, state, local_completed_at, retain_until
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'local_no_change', at,
       GREATEST(at + INTERVAL '8380800 seconds', sqlc.narg(retain_until)::timestamptz)
FROM completion
RETURNING *;

-- Owner is mandatory, including after auth-key rebinding. Semantic request
-- matching and saved-result disclosure belong to the subsequent slice.
-- name: MessageDeleteReceiptByOwnerIdentity :one
SELECT * FROM message_delete_receipts
WHERE owner_id = $1 AND auth_key_id = $2 AND session_id = $3 AND msg_id = $4;

-- This inert reader selects only request-matching fields and the local result
-- shape. It deliberately omits the transport tuple, timestamps and retention.
-- name: MessageDeleteReceiptForLookup :one
SELECT method, id_space, revoke, target_ids, pts, pts_count, receipt_key, state,
       operation_key, epoch, stream_id, position
FROM message_delete_receipts
WHERE owner_id = $1 AND auth_key_id = $2 AND session_id = $3 AND msg_id = $4;

-- All retained states count, including terminal rows eligible but not removed.
-- name: MessageDeleteReceiptOwnerCount :one
SELECT count(*)::bigint AS total FROM message_delete_receipts WHERE owner_id = $1;

-- Refusal admission and saturating increments belong to the composition slice.
-- name: MessageDeleteCapacityRefusalByOwner :one
SELECT total FROM message_delete_capacity_refusals WHERE owner_id = $1;

-- Count only committed authorized capacity refusals; saturate without overflow.
-- name: IncrementMessageDeleteCapacityRefusal :one
INSERT INTO message_delete_capacity_refusals (owner_id, total)
VALUES ($1, 1)
ON CONFLICT (owner_id) DO UPDATE
SET total = CASE
    WHEN message_delete_capacity_refusals.total < 9223372036854775807
    THEN message_delete_capacity_refusals.total + 1
    ELSE message_delete_capacity_refusals.total
END
RETURNING total;

-- Test-only inert policy probes. Inventory is explicit input because no
-- recovery-inventory reader or runtime caller exists in this slice. The
-- optional nanosecond test clock preserves unit-clock boundaries; NULL uses
-- the database transaction clock, whose timestamptz precision is microseconds.
-- name: MessageDeleteReceiptRetentionEligible :one
WITH receipt AS (
    SELECT stored.state, stored.confirmed_at, stored.local_completed_at, stored.retain_until,
           COALESCE(
               sqlc.narg(test_clock_unix_nano)::bigint::numeric,
               EXTRACT(EPOCH FROM transaction_timestamp()) * 1000000000
           ) AS database_now_unix_nano
    FROM message_delete_receipts AS stored
    WHERE stored.owner_id = sqlc.arg(owner_id)
      AND stored.auth_key_id = sqlc.arg(auth_key_id)
      AND stored.session_id = sqlc.arg(session_id)
      AND stored.msg_id = sqlc.arg(msg_id)
)
SELECT CASE
    WHEN receipt.state IN ('confirmed', 'local_no_change')
         AND sqlc.arg(inventory_outcome)::text = 'known'
    THEN receipt.database_now_unix_nano > EXTRACT(EPOCH FROM receipt.retain_until) * 1000000000
         AND receipt.database_now_unix_nano > EXTRACT(EPOCH FROM (
             COALESCE(receipt.confirmed_at, receipt.local_completed_at)
             + GREATEST(97, sqlc.arg(inventory_days)::integer) * INTERVAL '86400 seconds'
         )) * 1000000000
    ELSE false
END AS eligible
FROM receipt;

-- Only known inventory can extend a terminal deadline. Retention uses elapsed
-- 24-hour days, independent of the database session timezone and DST. The UPDATE predicate
-- compares against the stored row, so a shorter/equal request never rewrites
-- it and concurrent extensions cannot shorten an already-extended deadline.
-- Test-only inert helper; no runtime caller.
-- name: ExtendMessageDeleteReceiptRetention :one
WITH requested AS MATERIALIZED (
    SELECT stored.owner_id, stored.auth_key_id, stored.session_id, stored.msg_id,
           stored.state, stored.retain_until,
           sqlc.arg(inventory_outcome)::text AS inventory_outcome,
           CASE
               WHEN sqlc.arg(inventory_outcome)::text = 'known'
                    AND stored.state IN ('confirmed', 'local_no_change')
               THEN COALESCE(stored.confirmed_at, stored.local_completed_at)
                    + GREATEST(97, sqlc.arg(inventory_days)::integer) * INTERVAL '86400 seconds'
           END AS requested_until
    FROM message_delete_receipts AS stored
    WHERE stored.owner_id = sqlc.arg(owner_id)
      AND stored.auth_key_id = sqlc.arg(auth_key_id)
      AND stored.session_id = sqlc.arg(session_id)
      AND stored.msg_id = sqlc.arg(msg_id)
), extended AS (
    UPDATE message_delete_receipts AS receipt
    SET retain_until = requested.requested_until
    FROM requested
    WHERE receipt.owner_id = requested.owner_id
      AND receipt.auth_key_id = requested.auth_key_id
      AND receipt.session_id = requested.session_id
      AND receipt.msg_id = requested.msg_id
      AND receipt.state = requested.state
      AND requested.state IN ('confirmed', 'local_no_change')
      AND requested.inventory_outcome = 'known'
      AND requested.requested_until > receipt.retain_until
    RETURNING receipt.retain_until
)
SELECT CASE
           WHEN requested.inventory_outcome = 'unknown' THEN 'refused_unknown'
           WHEN requested.inventory_outcome = 'conflicting' THEN 'refused_conflicting'
           WHEN requested.state = 'pending' THEN 'pending'
           WHEN extended.retain_until IS NOT NULL THEN 'extended'
           ELSE 'unchanged'
       END AS outcome,
       requested.state,
       COALESCE(extended.retain_until, requested.retain_until) AS retain_until
FROM requested
LEFT JOIN extended ON true;

-- Test-only inert removal; no runtime caller. Every eligibility condition is
-- part of the DELETE predicate, so a concurrent extension committed before the
-- row lock is rechecked against the new row version and leaves it intact.
-- Takes no owner, poll or stream lock: an uncommitted removal stays counted.
-- name: RemoveEligibleMessageDeleteReceipt :execrows
DELETE FROM message_delete_receipts AS receipt
WHERE receipt.owner_id = sqlc.arg(owner_id)
  AND receipt.auth_key_id = sqlc.arg(auth_key_id)
  AND receipt.session_id = sqlc.arg(session_id)
  AND receipt.msg_id = sqlc.arg(msg_id)
  AND CASE
      WHEN receipt.state IN ('confirmed', 'local_no_change')
           AND sqlc.arg(inventory_outcome)::text = 'known'
      THEN COALESCE(
               sqlc.narg(test_clock_unix_nano)::bigint::numeric,
               EXTRACT(EPOCH FROM transaction_timestamp()) * 1000000000
           ) > GREATEST(
               EXTRACT(EPOCH FROM receipt.retain_until) * 1000000000,
               EXTRACT(EPOCH FROM (
                   COALESCE(receipt.confirmed_at, receipt.local_completed_at)
                   + GREATEST(97, sqlc.arg(inventory_days)::integer) * INTERVAL '86400 seconds'
               )) * 1000000000
           )
      ELSE false
  END;
