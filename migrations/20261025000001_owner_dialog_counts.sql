CREATE TABLE public.owner_dialog_counts (
    owner_id BIGINT PRIMARY KEY REFERENCES public.users (id) ON DELETE CASCADE,
    dialog_count BIGINT NOT NULL DEFAULT 0 CHECK (dialog_count >= 0),
    initialized BOOLEAN NOT NULL DEFAULT FALSE
);

-- A missing or uninitialized counter is reconciled from dialogs while holding
-- its row lock. Once initialized, writes use an atomic delta on that same row.
CREATE FUNCTION public.apply_owner_dialog_count_delta(p_owner_id BIGINT, p_delta BIGINT)
RETURNS VOID
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
DECLARE
    count_initialized BOOLEAN;
    actual_count BIGINT;
BEGIN
    INSERT INTO public.owner_dialog_counts (owner_id, dialog_count, initialized)
    SELECT p_owner_id, 0, FALSE
    WHERE EXISTS (SELECT 1 FROM public.users WHERE id = p_owner_id)
    ON CONFLICT (owner_id) DO NOTHING;

    SELECT counts.initialized
    INTO count_initialized
    FROM public.owner_dialog_counts AS counts
    WHERE counts.owner_id = p_owner_id
    FOR UPDATE;

    IF NOT FOUND THEN
        -- The user may be in the process of being deleted by a cascading FK.
        RETURN;
    END IF;

    IF count_initialized THEN
        IF p_delta <> 0 THEN
            UPDATE public.owner_dialog_counts
            SET dialog_count = dialog_count + p_delta
            WHERE owner_id = p_owner_id;
        END IF;
        RETURN;
    END IF;

    SELECT count(*)::BIGINT
    INTO actual_count
    FROM public.dialogs
    WHERE owner_id = p_owner_id;

    UPDATE public.owner_dialog_counts
    SET dialog_count = actual_count,
        initialized = TRUE
    WHERE owner_id = p_owner_id;
END;
$function$;

CREATE FUNCTION public.track_owner_dialog_count()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM public.apply_owner_dialog_count_delta(NEW.owner_id, 1);
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM public.apply_owner_dialog_count_delta(OLD.owner_id, -1);
        RETURN OLD;
    ELSIF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
        -- Keep two-owner updates on a stable counter-row lock order.
        IF OLD.owner_id < NEW.owner_id THEN
            PERFORM public.apply_owner_dialog_count_delta(OLD.owner_id, -1);
            PERFORM public.apply_owner_dialog_count_delta(NEW.owner_id, 1);
        ELSE
            PERFORM public.apply_owner_dialog_count_delta(NEW.owner_id, 1);
            PERFORM public.apply_owner_dialog_count_delta(OLD.owner_id, -1);
        END IF;
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER dialogs_track_owner_count
AFTER INSERT OR DELETE OR UPDATE OF owner_id ON public.dialogs
FOR EACH ROW
EXECUTE FUNCTION public.track_owner_dialog_count();
