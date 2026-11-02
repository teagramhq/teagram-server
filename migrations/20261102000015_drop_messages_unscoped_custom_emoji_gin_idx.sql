-- The owner-scoped GIN index supersedes the unscoped message-history index.
-- atlas:txmode none

DROP INDEX CONCURRENTLY messages_custom_emoji_document_ids_gin_idx;
