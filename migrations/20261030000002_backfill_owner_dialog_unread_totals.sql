-- Initialize accounts in committed per-owner steps so live dialog writes do
-- not hold a counter-row lock behind a batch of retained counter-row locks.
-- New accounts initialize both totals in the users trigger.
-- atlas:txmode none

CREATE OR REPLACE PROCEDURE public.backfill_owner_dialog_unread_totals()
LANGUAGE plpgsql
AS $procedure$
DECLARE
    last_user_id BIGINT := 0;
    batch_user_ids BIGINT[];
    user_id BIGINT;
BEGIN
    LOOP
        SELECT array_agg(batch.id ORDER BY batch.id)
        INTO batch_user_ids
        FROM (
            SELECT users.id
            FROM public.users AS users
            WHERE users.id > last_user_id
            ORDER BY users.id
            LIMIT 500
        ) AS batch;

        EXIT WHEN batch_user_ids IS NULL;

        FOREACH user_id IN ARRAY batch_user_ids LOOP
            PERFORM public.apply_owner_dialog_totals_delta(user_id, 0, 0);
            COMMIT;
        END LOOP;

        last_user_id := batch_user_ids[array_length(batch_user_ids, 1)];
    END LOOP;
END
$procedure$;

CALL public.backfill_owner_dialog_unread_totals();

DROP PROCEDURE public.backfill_owner_dialog_unread_totals();
