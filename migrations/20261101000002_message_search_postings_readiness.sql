-- atlas:txmode file

-- The setup migration installs and commits every message-write trigger before
-- this snapshot. This read scans message history without holding the DDL lock
-- that installs those triggers, so ordinary message writes continue.
-- Only mark dialogs with a live message lexeme missing from postings; writes
-- after trigger installation are already represented and remain ready.
INSERT INTO public.message_search_unready_dialogs (owner_id, peer_type, peer_id)
SELECT DISTINCT message.owner_id, message.peer_type, message.peer_id
FROM public.messages AS message
WHERE NOT message.deleted
  AND EXISTS (
      SELECT 1
      FROM unnest(tsvector_to_array(message.message_tsv)) AS term(lexeme)
      WHERE NOT EXISTS (
          SELECT 1
          FROM public.message_search_postings AS posting
          WHERE posting.owner_id = message.owner_id
            AND posting.local_id = message.local_id
            AND posting.lexeme = term.lexeme
      )
  )
ON CONFLICT (owner_id, peer_type, peer_id) DO NOTHING;
