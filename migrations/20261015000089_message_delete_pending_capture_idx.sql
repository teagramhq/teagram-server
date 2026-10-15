-- Confirmation holds the stream claim while finding its pending receipt.
-- Index the full capture reference so retained history from other owners is
-- never scanned. Build concurrently to keep receipt writes available.
-- Additive and reversible by dropping this index; receipt data is unchanged.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY message_delete_receipts_pending_capture_idx
    ON message_delete_receipts (operation_key, epoch, stream_id, position)
    WHERE state = 'pending';
