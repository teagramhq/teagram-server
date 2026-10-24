-- Snapshot existing keys before adding the marker column so new keys remain unmarked.

DO $snapshot$
BEGIN
    IF pg_catalog.to_regclass('public.auth_keys_ever_bound_backfill_snapshot') IS NULL THEN
        EXECUTE 'CREATE TABLE public.auth_keys_ever_bound_backfill_snapshot (id BIGINT PRIMARY KEY)';
        EXECUTE 'INSERT INTO public.auth_keys_ever_bound_backfill_snapshot (id) SELECT id FROM public.auth_keys';
    END IF;
END
$snapshot$;

ALTER TABLE public.auth_keys
    ADD COLUMN IF NOT EXISTS ever_bound_at TIMESTAMPTZ NULL;
