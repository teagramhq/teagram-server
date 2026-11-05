ALTER TABLE users
    ADD COLUMN emoji_status_document_id BIGINT NULL;

ALTER TABLE chat_available_reactions
    ADD COLUMN allow_custom BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN custom_emoji_ids BIGINT[] NOT NULL DEFAULT '{}'::bigint[],
    ADD CONSTRAINT chat_available_reactions_allow_custom_mode_check
        CHECK (mode = 'all' OR NOT allow_custom) NOT VALID,
    ADD CONSTRAINT chat_available_reactions_custom_ids_mode_check
        CHECK (mode = 'some' OR cardinality(custom_emoji_ids) = 0) NOT VALID;
ALTER TABLE chat_available_reactions
    VALIDATE CONSTRAINT chat_available_reactions_allow_custom_mode_check;
ALTER TABLE chat_available_reactions
    VALIDATE CONSTRAINT chat_available_reactions_custom_ids_mode_check;

ALTER TABLE channel_available_reactions
    ADD COLUMN allow_custom BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN custom_emoji_ids BIGINT[] NOT NULL DEFAULT '{}'::bigint[],
    ADD CONSTRAINT channel_available_reactions_allow_custom_mode_check
        CHECK (mode = 'all' OR NOT allow_custom) NOT VALID,
    ADD CONSTRAINT channel_available_reactions_custom_ids_mode_check
        CHECK (mode = 'some' OR cardinality(custom_emoji_ids) = 0) NOT VALID;
ALTER TABLE channel_available_reactions
    VALIDATE CONSTRAINT channel_available_reactions_allow_custom_mode_check;
ALTER TABLE channel_available_reactions
    VALIDATE CONSTRAINT channel_available_reactions_custom_ids_mode_check;
