-- CHECKs validate only the new row. A terminal no-change result must also
-- preserve its previous state and completion anchor across complete updates.
-- Validation only: no row writes, backfill or runtime caller. Reversible by
-- dropping this trigger and function without changing receipt data.

CREATE FUNCTION message_delete_receipts_guard_no_change()
RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state = 'local_no_change'
       AND (NEW.state IS DISTINCT FROM OLD.state
            OR NEW.local_completed_at IS DISTINCT FROM OLD.local_completed_at) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            CONSTRAINT = 'message_delete_receipts_no_change_immutable',
            MESSAGE = 'local no-change receipt state and completion anchor cannot change';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER message_delete_receipts_guard_no_change
BEFORE UPDATE ON message_delete_receipts
FOR EACH ROW EXECUTE FUNCTION message_delete_receipts_guard_no_change();
