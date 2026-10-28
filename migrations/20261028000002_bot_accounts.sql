ALTER TABLE users
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'human'
        CHECK (kind IN ('human', 'bot'));

ALTER TABLE users
    ADD CONSTRAINT users_id_kind_key UNIQUE (id, kind);

CREATE FUNCTION reject_user_kind_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'user kind is immutable'
        USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER users_kind_immutable
    BEFORE UPDATE OF kind ON users
    FOR EACH ROW
    EXECUTE FUNCTION reject_user_kind_update();

-- Fixed generated kinds make the binding restriction a composite FK, including
-- when a bot row and an auth-key update race each other.
ALTER TABLE auth_keys
    ADD COLUMN user_kind TEXT GENERATED ALWAYS AS ('human'::text) STORED,
    ADD COLUMN pending_user_kind TEXT GENERATED ALWAYS AS ('human'::text) STORED,
    ADD CONSTRAINT auth_keys_user_human_fk
        FOREIGN KEY (user_id, user_kind) REFERENCES users (id, kind),
    ADD CONSTRAINT auth_keys_pending_user_human_fk
        FOREIGN KEY (pending_user_id, pending_user_kind) REFERENCES users (id, kind);

-- A fixed generated bot kind prevents token digests from attaching to humans.
CREATE TABLE bot_tokens (
    user_id BIGINT PRIMARY KEY,
    user_kind TEXT GENERATED ALWAYS AS ('bot'::text) STORED,
    token_hash BYTEA,
    issued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ,
    FOREIGN KEY (user_id, user_kind) REFERENCES users (id, kind) ON DELETE CASCADE,
    CHECK (token_hash IS NULL OR octet_length(token_hash) = 32),
    CHECK (
        (revoked_at IS NULL AND token_hash IS NOT NULL)
        OR (revoked_at IS NOT NULL AND token_hash IS NULL)
    )
);
