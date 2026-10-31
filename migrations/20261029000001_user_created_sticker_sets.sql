CREATE FUNCTION user_created_sticker_keywords_within_limits(keywords_text text)
RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT
AS $$
    SELECT cardinality(string_to_array(keywords_text, ',')) <= 20
       AND NOT EXISTS (
           SELECT 1
           FROM unnest(string_to_array(keywords_text, ',')) AS keyword(value)
           WHERE octet_length(keyword.value) > 64
       )
$$;

CREATE TABLE user_created_sticker_sets (
    id          BIGSERIAL PRIMARY KEY CHECK (id > 0),
    access_hash BIGINT NOT NULL UNIQUE CHECK (access_hash <> 0),
    creator_id  BIGINT NOT NULL REFERENCES users (id),
    title       TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 64),
    short_name  TEXT NOT NULL CHECK (short_name ~ '^[A-Za-z][A-Za-z0-9_]{0,63}$'),
    software    TEXT NOT NULL DEFAULT '' CHECK (octet_length(software) <= 256),
    kind        TEXT NOT NULL CHECK (kind IN ('regular', 'mask', 'emojis')),
    text_color  BOOL NOT NULL DEFAULT false,
    is_live     BOOL NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);

CREATE UNIQUE INDEX user_created_sticker_sets_short_name_key
    ON user_created_sticker_sets (lower(short_name));
CREATE INDEX user_created_sticker_sets_creator_idx
    ON user_created_sticker_sets (creator_id, id);

CREATE TABLE user_created_sticker_set_items (
    set_id       BIGINT NOT NULL REFERENCES user_created_sticker_sets (id) ON DELETE CASCADE,
    position     INT NOT NULL CHECK (position >= 0),
    file_id      BIGINT NOT NULL REFERENCES files (id) ON DELETE RESTRICT,
    emoji        TEXT NOT NULL
                 CONSTRAINT user_created_sticker_set_items_emoji_limit CHECK (char_length(emoji) BETWEEN 1 AND 20),
    keywords     TEXT NOT NULL DEFAULT ''
                 CONSTRAINT user_created_sticker_set_items_keywords_limit CHECK (
                     user_created_sticker_keywords_within_limits(keywords)
                 ),
    format       TEXT NOT NULL CHECK (format IN ('WEBP', 'TGS', 'WEBM')),
    width        INT NOT NULL CHECK (width > 0),
    height       INT NOT NULL CHECK (height > 0),
    duration_ns  BIGINT NOT NULL DEFAULT 0 CHECK (duration_ns >= 0),
    mask_coords  JSONB,
    PRIMARY KEY (set_id, position),
    UNIQUE (file_id)
);

-- User sticker documents use their positive files.id in sticker_document_id;
-- catalogue documents keep their negative, separate identity. Both remain
-- distinct from messages.file_id so uploaded files acquire no message reference
-- merely because they are named as stickers.
ALTER TABLE messages
    DROP CONSTRAINT messages_sticker_document_separate_check,
    ADD CONSTRAINT messages_sticker_document_separate_check
        CHECK (sticker_document_id IS NULL OR (sticker_document_id <> 0 AND file_id = 0)) NOT VALID;

ALTER TABLE channel_messages
    DROP CONSTRAINT channel_messages_sticker_document_separate_check,
    ADD CONSTRAINT channel_messages_sticker_document_separate_check
        CHECK (sticker_document_id IS NULL OR (sticker_document_id <> 0 AND file_id IS NULL)) NOT VALID;
