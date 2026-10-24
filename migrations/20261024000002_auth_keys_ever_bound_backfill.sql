-- Backfill existing keys in committed batches so a failed run can resume.
-- atlas:txmode none

DO $migration$
DECLARE
    backfill_complete BOOLEAN;
BEGIN
    SELECT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.auth_keys'::regclass
          AND conname = 'auth_keys_user_requires_ever_bound'
          AND convalidated
    )
    INTO backfill_complete;

    IF backfill_complete THEN
        IF pg_catalog.to_regclass('public.auth_keys_ever_bound_backfill_snapshot') IS NOT NULL THEN
            DROP TABLE public.auth_keys_ever_bound_backfill_snapshot;
            COMMIT;
        END IF;
        RETURN;
    END IF;

    COMMIT;

    LOOP
        WITH batch AS MATERIALIZED (
            SELECT auth_key.id
            FROM public.auth_keys AS auth_key
            INNER JOIN public.auth_keys_ever_bound_backfill_snapshot AS existing_key
                ON existing_key.id = auth_key.id
            WHERE auth_key.ever_bound_at IS NULL
            ORDER BY auth_key.id
            LIMIT 10000
        )
        UPDATE public.auth_keys AS auth_key
        SET ever_bound_at = auth_key.created_at
        FROM batch
        WHERE auth_key.id = batch.id
          AND auth_key.ever_bound_at IS NULL;
        COMMIT;

        EXIT WHEN NOT EXISTS (
            SELECT 1
            FROM public.auth_keys AS auth_key
            INNER JOIN public.auth_keys_ever_bound_backfill_snapshot AS existing_key
                ON existing_key.id = auth_key.id
            WHERE auth_key.ever_bound_at IS NULL
            LIMIT 1
        );
    END LOOP;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_catalog.pg_constraint
        WHERE conrelid = 'public.auth_keys'::regclass
          AND conname = 'auth_keys_user_requires_ever_bound'
    ) THEN
        ALTER TABLE public.auth_keys
            ADD CONSTRAINT auth_keys_user_requires_ever_bound
            CHECK (user_id IS NULL OR ever_bound_at IS NOT NULL) NOT VALID;
    END IF;
    COMMIT;

    ALTER TABLE public.auth_keys
        VALIDATE CONSTRAINT auth_keys_user_requires_ever_bound;
    COMMIT;

    DROP TABLE public.auth_keys_ever_bound_backfill_snapshot;
    COMMIT;
END
$migration$;
