-- Include accounts without dialogs so the exact count lookup always uses the
-- owner's primary-key row. New accounts are initialized by the users trigger.
-- Commit each owner separately so live two-owner writes cannot deadlock with a
-- batch of retained counter-row locks.
-- atlas:txmode none

DO $migration$
DECLARE
    last_owner_id BIGINT := 0;
    batch_owner_ids BIGINT[];
    owner_id BIGINT;
BEGIN
    LOOP
        SELECT array_agg(batch.id ORDER BY batch.id)
        INTO batch_owner_ids
        FROM (
            SELECT users.id
            FROM public.users AS users
            WHERE users.id > last_owner_id
            ORDER BY users.id
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
