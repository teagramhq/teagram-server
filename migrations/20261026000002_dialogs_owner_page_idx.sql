-- Dialog pages are ordered by recent activity; the primary key is ordered by
-- peer and cannot stop after the requested page for a large owner.
-- The concurrent build avoids blocking dialog writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY dialogs_owner_page_idx
    ON public.dialogs (owner_id, top_message DESC, peer_type, peer_id);
