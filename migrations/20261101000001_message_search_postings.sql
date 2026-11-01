-- atlas:txmode file

-- Keep ordered per-dialog lexeme postings for messages.search. Existing message
-- history is intentionally not copied here; those dialogs stay on the legacy
-- query until a separate initializer has populated their postings.
CREATE TABLE public.message_search_postings (
    owner_id  BIGINT   NOT NULL,
    local_id  BIGINT   NOT NULL,
    peer_type SMALLINT NOT NULL,
    peer_id   BIGINT   NOT NULL,
    lexeme    TEXT     NOT NULL,
    PRIMARY KEY (owner_id, local_id, lexeme),
    FOREIGN KEY (owner_id, local_id)
        REFERENCES public.messages (owner_id, local_id) ON DELETE CASCADE
);

CREATE TABLE public.message_search_posting_counts (
    owner_id      BIGINT NOT NULL,
    peer_type     SMALLINT NOT NULL,
    peer_id       BIGINT NOT NULL,
    lexeme        TEXT NOT NULL,
    posting_count BIGINT NOT NULL CHECK (posting_count >= 0),
    PRIMARY KEY (owner_id, peer_type, peer_id, lexeme)
);

CREATE INDEX message_search_postings_dialog_page_idx
    ON public.message_search_postings (owner_id, peer_type, peer_id, lexeme, local_id DESC);

CREATE TABLE public.message_search_unready_dialogs (
    owner_id  BIGINT   NOT NULL,
    peer_type SMALLINT NOT NULL,
    peer_id   BIGINT   NOT NULL,
    PRIMARY KEY (owner_id, peer_type, peer_id)
);

CREATE FUNCTION public.track_message_search_posting_count()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
DECLARE
    remaining_count BIGINT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO public.message_search_posting_counts AS counts (
            owner_id, peer_type, peer_id, lexeme, posting_count
        )
        VALUES (NEW.owner_id, NEW.peer_type, NEW.peer_id, NEW.lexeme, 1)
        ON CONFLICT (owner_id, peer_type, peer_id, lexeme)
        DO UPDATE SET posting_count = counts.posting_count + 1;
        RETURN NEW;
    END IF;

    UPDATE public.message_search_posting_counts
    SET posting_count = posting_count - 1
    WHERE owner_id = OLD.owner_id
      AND peer_type = OLD.peer_type
      AND peer_id = OLD.peer_id
      AND lexeme = OLD.lexeme
    RETURNING posting_count INTO remaining_count;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'message search posting count missing for %/%/%/%',
            OLD.owner_id, OLD.peer_type, OLD.peer_id, OLD.lexeme;
    END IF;

    IF remaining_count = 0 THEN
        DELETE FROM public.message_search_posting_counts
        WHERE owner_id = OLD.owner_id
          AND peer_type = OLD.peer_type
          AND peer_id = OLD.peer_id
          AND lexeme = OLD.lexeme;
    END IF;

    RETURN OLD;
END;
$function$;

CREATE TRIGGER message_search_posting_counts_after_insert
AFTER INSERT ON public.message_search_postings
FOR EACH ROW EXECUTE FUNCTION public.track_message_search_posting_count();

CREATE TRIGGER message_search_posting_counts_after_delete
AFTER DELETE ON public.message_search_postings
FOR EACH ROW EXECUTE FUNCTION public.track_message_search_posting_count();

CREATE FUNCTION public.sync_message_search_postings()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $function$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        DELETE FROM public.message_search_postings
        WHERE owner_id = OLD.owner_id AND local_id = OLD.local_id;
    END IF;

    IF NOT NEW.deleted THEN
        INSERT INTO public.message_search_postings (owner_id, local_id, peer_type, peer_id, lexeme)
        SELECT NEW.owner_id, NEW.local_id, NEW.peer_type, NEW.peer_id, term.lexeme
        FROM unnest(tsvector_to_array(NEW.message_tsv)) AS term(lexeme);
    END IF;

    RETURN NEW;
END;
$function$;

CREATE TRIGGER messages_search_postings_after_insert
AFTER INSERT ON public.messages
FOR EACH ROW EXECUTE FUNCTION public.sync_message_search_postings();

CREATE TRIGGER messages_search_postings_after_update
AFTER UPDATE OF message, deleted, peer_type, peer_id ON public.messages
FOR EACH ROW
WHEN (
    OLD.message_tsv IS DISTINCT FROM NEW.message_tsv
    OR OLD.deleted IS DISTINCT FROM NEW.deleted
    OR OLD.peer_type IS DISTINCT FROM NEW.peer_type
    OR OLD.peer_id IS DISTINCT FROM NEW.peer_id
)
EXECUTE FUNCTION public.sync_message_search_postings();
