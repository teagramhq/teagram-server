ALTER TABLE public.auth_keys
    ADD COLUMN IF NOT EXISTS ever_bound_at TIMESTAMPTZ NULL;

-- Keep the invariant database-enforced for old and new server versions.
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

-- Capture inserts after this migration commits so the next migration can
-- distinguish keys created during the rollout from the preexisting cohort.
CREATE TABLE public.auth_keys_ever_bound_backfill_new_keys (
    id BIGINT PRIMARY KEY
);

CREATE FUNCTION public.auth_keys_capture_ever_bound_backfill_new_key()
RETURNS TRIGGER
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $function$
BEGIN
    INSERT INTO public.auth_keys_ever_bound_backfill_new_keys (id)
    VALUES (NEW.id)
    ON CONFLICT (id) DO NOTHING;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER auth_keys_capture_ever_bound_backfill_new_key
AFTER INSERT ON public.auth_keys
FOR EACH ROW
EXECUTE FUNCTION public.auth_keys_capture_ever_bound_backfill_new_key();
