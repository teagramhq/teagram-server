-- The channel-scoped GIN index supersedes the unscoped message-history index.
-- atlas:txmode none

DROP INDEX CONCURRENTLY channel_messages_custom_emoji_document_ids_gin_idx;
