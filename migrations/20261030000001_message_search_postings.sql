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

CREATE INDEX message_search_postings_dialog_page_idx
    ON public.message_search_postings (owner_id, peer_type, peer_id, lexeme, local_id DESC);

CREATE TABLE public.message_search_unready_dialogs (
    owner_id  BIGINT   NOT NULL,
    peer_type SMALLINT NOT NULL,
    peer_id   BIGINT   NOT NULL,
    PRIMARY KEY (owner_id, peer_type, peer_id)
);

-- Record only which dialogs still need the follow-up initializer; this does
-- not copy message text or lexemes into the postings table.
INSERT INTO public.message_search_unready_dialogs (owner_id, peer_type, peer_id)
SELECT DISTINCT owner_id, peer_type, peer_id
FROM public.messages;

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
