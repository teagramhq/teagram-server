CREATE FUNCTION public.initialize_owner_dialog_count_for_user()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    INSERT INTO public.owner_dialog_counts (owner_id, dialog_count, initialized)
    VALUES (NEW.id, 0, TRUE)
    ON CONFLICT (owner_id) DO NOTHING;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER users_initialize_owner_dialog_count
AFTER INSERT ON public.users
FOR EACH ROW
EXECUTE FUNCTION public.initialize_owner_dialog_count_for_user();
