-- Backfill existing owners in committed per-owner steps. This releases each
-- counter-row lock before moving to the next owner, so it cannot deadlock with a
-- live send that holds two owners in sender/recipient order.
-- atlas:txmode none

DO $migration$
DECLARE
    last_owner_id BIGINT := 0;
    batch_owner_ids BIGINT[];
    owner_id BIGINT;
BEGIN
    LOOP
        SELECT array_agg(batch.owner_id ORDER BY batch.owner_id)
        INTO batch_owner_ids
        FROM (
            SELECT dialogs.owner_id
            FROM public.dialogs AS dialogs
            WHERE dialogs.owner_id > last_owner_id
            GROUP BY dialogs.owner_id
            ORDER BY dialogs.owner_id
            LIMIT 500
        ) AS batch;

        EXIT WHEN batch_owner_ids IS NULL;

        FOREACH owner_id IN ARRAY batch_owner_ids LOOP
            PERFORM public.apply_owner_dialog_count_delta(owner_id, 0);
            COMMIT;
        END LOOP;

        last_owner_id := batch_owner_ids[array_length(batch_owner_ids, 1)];
    END LOOP;
END
$migration$;
