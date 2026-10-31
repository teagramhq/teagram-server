-- atlas:txmode file

-- The setup migration installs and commits every message-write trigger before
-- this snapshot. This read scans message history without holding the DDL lock
-- that installs those triggers, so ordinary message writes continue.
INSERT INTO public.message_search_unready_dialogs (owner_id, peer_type, peer_id)
SELECT DISTINCT owner_id, peer_type, peer_id
FROM public.messages
ON CONFLICT (owner_id, peer_type, peer_id) DO NOTHING;
