-- Persist the last committed message range so a rerun resumes an incomplete
-- dialog without rebuilding already committed chunks.
ALTER TABLE public.message_search_unready_dialogs
    ADD COLUMN last_local_id BIGINT NOT NULL DEFAULT 0;

-- Serialize count-row changes for a dialog's affected lexemes in one stable
-- order. Hash collisions only serialize unrelated lexemes; the lock key still
-- includes the complete dialog and lexeme identity before it is hashed.
CREATE FUNCTION public.lock_message_search_posting_lexemes(
    p_owner_id BIGINT,
    p_peer_type SMALLINT,
    p_peer_id BIGINT,
    p_lexemes TEXT[]
)
RETURNS BOOLEAN
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
DECLARE
    posting_lexeme TEXT;
BEGIN
    FOR posting_lexeme IN
        SELECT DISTINCT candidate.lexeme COLLATE "C"
        FROM unnest(COALESCE(p_lexemes, ARRAY[]::TEXT[])) AS candidate(lexeme)
        ORDER BY candidate.lexeme COLLATE "C"
    LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(
            'message-search-posting-count/v1:'
                || p_owner_id::TEXT || ':'
                || p_peer_type::TEXT || ':'
                || p_peer_id::TEXT || ':'
                || posting_lexeme,
            73194627
        ));
    END LOOP;
    RETURN TRUE;
END;
$function$;

CREATE OR REPLACE FUNCTION public.sync_message_search_postings()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
DECLARE
    old_lexemes TEXT[];
    new_lexemes TEXT[];
BEGIN
    IF TG_OP = 'UPDATE' THEN
        SELECT COALESCE(array_agg(lexeme ORDER BY lexeme COLLATE "C"), ARRAY[]::TEXT[])
        INTO old_lexemes
        FROM (
            SELECT term.lexeme
            FROM unnest(tsvector_to_array(OLD.message_tsv)) AS term(lexeme)
            UNION
            SELECT posting.lexeme
            FROM public.message_search_postings AS posting
            WHERE posting.owner_id = OLD.owner_id AND posting.local_id = OLD.local_id
        ) AS old_terms;
        new_lexemes := tsvector_to_array(NEW.message_tsv);

        IF (OLD.owner_id, OLD.peer_type, OLD.peer_id) = (NEW.owner_id, NEW.peer_type, NEW.peer_id) THEN
            SELECT COALESCE(array_agg(lexeme ORDER BY lexeme COLLATE "C"), ARRAY[]::TEXT[])
            INTO old_lexemes
            FROM (
                SELECT unnest(old_lexemes) AS lexeme
                UNION
                SELECT unnest(new_lexemes) AS lexeme
            ) AS affected_terms;
            PERFORM public.lock_message_search_posting_lexemes(NEW.owner_id, NEW.peer_type, NEW.peer_id, old_lexemes);
        ELSIF (OLD.owner_id, OLD.peer_type, OLD.peer_id) < (NEW.owner_id, NEW.peer_type, NEW.peer_id) THEN
            PERFORM public.lock_message_search_posting_lexemes(OLD.owner_id, OLD.peer_type, OLD.peer_id, old_lexemes);
            PERFORM public.lock_message_search_posting_lexemes(NEW.owner_id, NEW.peer_type, NEW.peer_id, new_lexemes);
        ELSE
            PERFORM public.lock_message_search_posting_lexemes(NEW.owner_id, NEW.peer_type, NEW.peer_id, new_lexemes);
            PERFORM public.lock_message_search_posting_lexemes(OLD.owner_id, OLD.peer_type, OLD.peer_id, old_lexemes);
        END IF;

        DELETE FROM public.message_search_postings
        WHERE owner_id = OLD.owner_id AND local_id = OLD.local_id;
    ELSE
        new_lexemes := tsvector_to_array(NEW.message_tsv);
        PERFORM public.lock_message_search_posting_lexemes(NEW.owner_id, NEW.peer_type, NEW.peer_id, new_lexemes);
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
DECLARE
    old_lexemes TEXT[];
BEGIN
    SELECT COALESCE(array_agg(lexeme ORDER BY lexeme COLLATE "C"), ARRAY[]::TEXT[])
    INTO old_lexemes
    FROM (
        SELECT term.lexeme
        FROM unnest(tsvector_to_array(OLD.message_tsv)) AS term(lexeme)
        UNION
        SELECT posting.lexeme
        FROM public.message_search_postings AS posting
        WHERE posting.owner_id = OLD.owner_id AND posting.local_id = OLD.local_id
    ) AS old_terms;

    PERFORM public.lock_message_search_posting_lexemes(OLD.owner_id, OLD.peer_type, OLD.peer_id, old_lexemes);
    RETURN OLD;
END;
$function$;

CREATE TRIGGER messages_search_postings_before_delete
BEFORE DELETE ON public.messages
FOR EACH ROW EXECUTE FUNCTION public.lock_message_search_postings_before_delete();
