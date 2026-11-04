-- Persist the last committed message range so a rerun resumes an incomplete
-- dialog without rebuilding already committed chunks.
ALTER TABLE public.message_search_unready_dialogs
    ADD COLUMN last_local_id BIGINT NOT NULL DEFAULT 0;
