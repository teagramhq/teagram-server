-- Initialize accounts in committed per-owner steps so live dialog writes do
-- not hold a counter-row lock behind a batch of retained counter-row locks.
-- New accounts initialize both totals in the users trigger. The CALL is the
-- file's only statement: a COMMIT inside it is rejected in a multi-statement
-- implicit transaction.
-- atlas:txmode none

CALL public.backfill_owner_dialog_unread_totals();
