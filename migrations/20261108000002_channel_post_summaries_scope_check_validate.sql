-- Validate the scope check added by the previous migration. It takes only a
-- SHARE UPDATE EXCLUSIVE lock, so channel writes keep flowing.
ALTER TABLE channel_post_summaries
    VALIDATE CONSTRAINT channel_post_summaries_scope_check;
