-- A terminal deadline may extend but never shrink, even above the CHECK floor.
-- Replace only the validator, preserving the no-change state/anchor protection.
-- No row rewrites or runtime writer. Reversible by restoring the previous
-- validator definition without changing receipt data or its trigger.

CREATE OR REPLACE FUNCTION message_delete_receipts_guard_no_change()
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
    IF OLD.state IN ('confirmed', 'local_no_change')
       AND (NEW.retain_until IS NULL OR NEW.retain_until < OLD.retain_until) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            CONSTRAINT = 'message_delete_receipts_retention_monotonic',
            MESSAGE = 'terminal receipt retention deadline cannot decrease';
    END IF;
    RETURN NEW;
END;
$$;
