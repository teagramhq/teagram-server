ALTER TABLE public.auth_keys
    ADD COLUMN IF NOT EXISTS ever_bound_at TIMESTAMPTZ NULL;

-- Keep binds from an older server version compatible while the rollout applies
-- the backfill and constraint. Install this in the same migration as the column
-- so no old-server write can pass between the two schema changes.
CREATE FUNCTION public.auth_keys_mark_ever_bound()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    IF NEW.user_id IS NOT NULL AND NEW.ever_bound_at IS NULL THEN
        NEW.ever_bound_at := pg_catalog.clock_timestamp();
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER auth_keys_mark_ever_bound
BEFORE INSERT OR UPDATE OF user_id ON public.auth_keys
FOR EACH ROW
EXECUTE FUNCTION public.auth_keys_mark_ever_bound();
