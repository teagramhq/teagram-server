ALTER TABLE public.owner_dialog_counts
    ADD COLUMN dialog_unread_total BIGINT NOT NULL DEFAULT 0
        CHECK (dialog_unread_total >= 0),
    ADD COLUMN dialog_unread_initialized BOOLEAN NOT NULL DEFAULT FALSE;

CREATE FUNCTION public.apply_owner_dialog_totals_delta(
    p_owner_id BIGINT,
    p_dialog_delta BIGINT,
    p_unread_delta BIGINT
)
RETURNS VOID
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
DECLARE
    count_initialized BOOLEAN;
    unread_initialized BOOLEAN;
    actual_dialog_count BIGINT;
    actual_unread_total BIGINT;
BEGIN
    INSERT INTO public.owner_dialog_counts (
        owner_id, dialog_count, initialized, dialog_unread_total, dialog_unread_initialized
    )
    SELECT p_owner_id, 0, FALSE, 0, FALSE
    WHERE EXISTS (SELECT 1 FROM public.users WHERE id = p_owner_id)
    ON CONFLICT (owner_id) DO NOTHING;

    SELECT counts.initialized, counts.dialog_unread_initialized
    INTO count_initialized, unread_initialized
    FROM public.owner_dialog_counts AS counts
    WHERE counts.owner_id = p_owner_id
    FOR UPDATE;

    IF NOT FOUND THEN
        -- The user may be in the process of being deleted by a cascading FK.
        RETURN;
    END IF;

    IF NOT count_initialized OR NOT unread_initialized THEN
        SELECT count(*)::BIGINT, COALESCE(sum(dialog.unread_count), 0)::BIGINT
        INTO actual_dialog_count, actual_unread_total
        FROM public.dialogs AS dialog
        WHERE dialog.owner_id = p_owner_id;

        UPDATE public.owner_dialog_counts
        SET dialog_count = CASE WHEN count_initialized THEN dialog_count ELSE actual_dialog_count END,
            initialized = TRUE,
            dialog_unread_total = CASE WHEN unread_initialized THEN dialog_unread_total ELSE actual_unread_total END,
            dialog_unread_initialized = TRUE
        WHERE owner_id = p_owner_id;
    END IF;

    IF (count_initialized AND p_dialog_delta <> 0)
       OR (unread_initialized AND p_unread_delta <> 0) THEN
        UPDATE public.owner_dialog_counts
        SET dialog_count = dialog_count + CASE WHEN count_initialized THEN p_dialog_delta ELSE 0 END,
            dialog_unread_total = dialog_unread_total + CASE WHEN unread_initialized THEN p_unread_delta ELSE 0 END
        WHERE owner_id = p_owner_id;
    END IF;
END;
$function$;

CREATE OR REPLACE FUNCTION public.initialize_owner_dialog_count_before_write()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM public.apply_owner_dialog_totals_delta(NEW.owner_id, 0, 0);
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM public.apply_owner_dialog_totals_delta(OLD.owner_id, 0, 0);
        RETURN OLD;
    ELSIF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
        -- Keep two-owner updates on a stable counter-row lock order.
        IF OLD.owner_id < NEW.owner_id THEN
            PERFORM public.apply_owner_dialog_totals_delta(OLD.owner_id, 0, 0);
            PERFORM public.apply_owner_dialog_totals_delta(NEW.owner_id, 0, 0);
        ELSE
            PERFORM public.apply_owner_dialog_totals_delta(NEW.owner_id, 0, 0);
            PERFORM public.apply_owner_dialog_totals_delta(OLD.owner_id, 0, 0);
        END IF;
    ELSE
        PERFORM public.apply_owner_dialog_totals_delta(NEW.owner_id, 0, 0);
    END IF;
    RETURN NEW;
END;
$function$;

DROP TRIGGER dialogs_initialize_owner_count_before_write ON public.dialogs;
CREATE TRIGGER dialogs_initialize_owner_count_before_write
BEFORE INSERT OR DELETE OR UPDATE OF owner_id, unread_count ON public.dialogs
FOR EACH ROW
EXECUTE FUNCTION public.initialize_owner_dialog_count_before_write();

CREATE OR REPLACE FUNCTION public.track_owner_dialog_count()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM public.apply_owner_dialog_totals_delta(NEW.owner_id, 1, NEW.unread_count);
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM public.apply_owner_dialog_totals_delta(OLD.owner_id, -1, -OLD.unread_count);
        RETURN OLD;
    ELSIF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
        -- Keep two-owner updates on a stable counter-row lock order.
        IF OLD.owner_id < NEW.owner_id THEN
            PERFORM public.apply_owner_dialog_totals_delta(OLD.owner_id, -1, -OLD.unread_count);
            PERFORM public.apply_owner_dialog_totals_delta(NEW.owner_id, 1, NEW.unread_count);
        ELSE
            PERFORM public.apply_owner_dialog_totals_delta(NEW.owner_id, 1, NEW.unread_count);
            PERFORM public.apply_owner_dialog_totals_delta(OLD.owner_id, -1, -OLD.unread_count);
        END IF;
    ELSE
        PERFORM public.apply_owner_dialog_totals_delta(
            NEW.owner_id,
            0,
            NEW.unread_count - OLD.unread_count
        );
    END IF;
    RETURN NEW;
END;
$function$;

DROP TRIGGER dialogs_track_owner_count ON public.dialogs;
CREATE TRIGGER dialogs_track_owner_count
AFTER INSERT OR DELETE OR UPDATE OF owner_id, unread_count ON public.dialogs
FOR EACH ROW
EXECUTE FUNCTION public.track_owner_dialog_count();

CREATE OR REPLACE FUNCTION public.initialize_owner_dialog_count_for_user()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    INSERT INTO public.owner_dialog_counts (
        owner_id, dialog_count, initialized, dialog_unread_total, dialog_unread_initialized
    )
    VALUES (NEW.id, 0, TRUE, 0, TRUE)
    ON CONFLICT (owner_id) DO NOTHING;
    RETURN NEW;
END;
$function$;

-- Called once by the next migration outside a transaction; each owner commits on
-- its own so live dialog writes never queue behind retained counter-row locks.
CREATE PROCEDURE public.backfill_owner_dialog_unread_totals()
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
