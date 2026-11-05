-- Persist the last committed message range so a rerun resumes an incomplete
-- dialog without rebuilding already committed chunks.
ALTER TABLE public.message_search_unready_dialogs
    ADD COLUMN last_local_id BIGINT NOT NULL DEFAULT 0;

-- Serialize posting updates with initialization only while a dialog is
-- unready. Markers are removed after the full backfill, so ready-dialog writes
-- do not contend on shared buckets. The bounded set avoids a lock-table entry
-- for every distinct dialog/lexeme in bulk writes.
CREATE FUNCTION public.lock_message_search_posting_dialogs(
    p_owner_ids BIGINT[],
    p_peer_types SMALLINT[],
    p_peer_ids BIGINT[]
)
RETURNS BOOLEAN
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
DECLARE
    lock_class INTEGER;
    lock_bucket INTEGER;
BEGIN
    lock_class := hashtext('message-search-posting-dialog/v1:' || current_database());

    FOR lock_bucket IN
        SELECT DISTINCT (
            (hashtextextended(
                'message-search-posting-dialog/v1:'
                    || dialog.owner_id::TEXT || ':'
                    || dialog.peer_type::TEXT || ':'
                    || dialog.peer_id::TEXT,
                73194627
            ) % 1024 + 1024) % 1024
        )::INTEGER AS bucket
        FROM unnest(
            COALESCE(p_owner_ids, ARRAY[]::BIGINT[]),
            COALESCE(p_peer_types, ARRAY[]::SMALLINT[]),
            COALESCE(p_peer_ids, ARRAY[]::BIGINT[])
        ) AS dialog(owner_id, peer_type, peer_id)
        JOIN public.message_search_unready_dialogs AS unready
          ON unready.owner_id = dialog.owner_id
         AND unready.peer_type = dialog.peer_type
         AND unready.peer_id = dialog.peer_id
        ORDER BY bucket
    LOOP
        PERFORM pg_advisory_xact_lock(lock_class, lock_bucket);
    END LOOP;
    RETURN TRUE;
END;
$function$;

-- A direct send writes sender and recipient copies in separate statements.
-- Lock both dialog buckets before inserting the first copy, in the helper's
-- sorted order, so overlapping bucket collisions cannot form a cycle.
CREATE FUNCTION public.lock_message_search_posting_pair_before_insert()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    IF NEW.peer_type = 1 THEN -- PeerTypeUser
        PERFORM public.lock_message_search_posting_dialogs(
            ARRAY[NEW.owner_id, NEW.peer_id],
            ARRAY[NEW.peer_type, NEW.peer_type],
            ARRAY[NEW.peer_id, NEW.owner_id]
        );
    ELSE
        PERFORM public.lock_message_search_posting_dialogs(
            ARRAY[NEW.owner_id], ARRAY[NEW.peer_type], ARRAY[NEW.peer_id]
        );
    END IF;
    RETURN NEW;
END;
$function$;

CREATE TRIGGER messages_search_posting_pair_before_insert
BEFORE INSERT ON public.messages
FOR EACH ROW EXECUTE FUNCTION public.lock_message_search_posting_pair_before_insert();

CREATE OR REPLACE FUNCTION public.sync_message_search_postings()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
DECLARE
    new_lexemes TEXT[];
BEGIN
    IF TG_OP = 'UPDATE' THEN
        new_lexemes := tsvector_to_array(NEW.message_tsv);
        PERFORM public.lock_message_search_posting_dialogs(
            ARRAY[OLD.owner_id, NEW.owner_id],
            ARRAY[OLD.peer_type, NEW.peer_type],
            ARRAY[OLD.peer_id, NEW.peer_id]
        );
        DELETE FROM public.message_search_postings
        WHERE owner_id = OLD.owner_id AND local_id = OLD.local_id;
    ELSE
        new_lexemes := tsvector_to_array(NEW.message_tsv);
        PERFORM public.lock_message_search_posting_dialogs(
            ARRAY[NEW.owner_id], ARRAY[NEW.peer_type], ARRAY[NEW.peer_id]
        );
    END IF;

    IF NOT NEW.deleted THEN
        INSERT INTO public.message_search_postings (owner_id, local_id, peer_type, peer_id, lexeme)
        SELECT NEW.owner_id, NEW.local_id, NEW.peer_type, NEW.peer_id, term.lexeme
        FROM unnest(new_lexemes) AS term(lexeme);
    END IF;

    RETURN NEW;
END;
$function$;

CREATE FUNCTION public.lock_message_search_postings_before_delete()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    PERFORM public.lock_message_search_posting_dialogs(
        ARRAY[OLD.owner_id], ARRAY[OLD.peer_type], ARRAY[OLD.peer_id]
    );
    RETURN OLD;
END;
$function$;

CREATE TRIGGER messages_search_postings_before_delete
BEFORE DELETE ON public.messages
FOR EACH ROW EXECUTE FUNCTION public.lock_message_search_postings_before_delete();
