-- Reconcile existing channels without holding a transaction or row locks across
-- the whole table. Participant triggers serialize changes on each counter row;
-- the count query runs after that row is locked, so a concurrent join or leave
-- is either included here or applied by its trigger after this batch commits.
-- atlas:txmode none

DO $migration$
DECLARE
    high_water_id BIGINT;
    last_channel_id BIGINT := 0;
    batch_end_id BIGINT;
    batch_channel_id BIGINT;
BEGIN
    SELECT max(channel_id)
      INTO high_water_id
      FROM channel_participant_counts;
    COMMIT;

    LOOP
        batch_end_id := NULL;
        FOR batch_channel_id IN
            SELECT count_row.channel_id
            FROM channel_participant_counts AS count_row
            WHERE count_row.channel_id > last_channel_id
              AND count_row.channel_id <= high_water_id
            ORDER BY count_row.channel_id
            LIMIT 500
            FOR UPDATE
        LOOP
            batch_end_id := batch_channel_id;
        END LOOP;

        EXIT WHEN batch_end_id IS NULL;

        UPDATE channel_participant_counts AS count_row
        SET participant_count = (
            SELECT count(*)::bigint
            FROM channel_participants AS participant
            WHERE participant.channel_id = count_row.channel_id
        )
        WHERE count_row.channel_id > last_channel_id
          AND count_row.channel_id <= batch_end_id;
        COMMIT;

        last_channel_id := batch_end_id;
    END LOOP;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.channel_participant_counts'::regclass
          AND conname = 'channel_participant_counts_nonnegative'
    ) THEN
        ALTER TABLE public.channel_participant_counts
            ADD CONSTRAINT channel_participant_counts_nonnegative
            CHECK (participant_count >= 0) NOT VALID;
    END IF;
    COMMIT;

    ALTER TABLE public.channel_participant_counts
        VALIDATE CONSTRAINT channel_participant_counts_nonnegative;
    COMMIT;
END;
$migration$;
