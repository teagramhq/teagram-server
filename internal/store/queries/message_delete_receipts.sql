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
