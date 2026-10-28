-- Account kind is immutable. Existing accounts remain human, and old writers
-- continue to create human accounts through the default.
ALTER TABLE users ADD COLUMN kind TEXT NOT NULL DEFAULT 'human';
ALTER TABLE users ADD CONSTRAINT users_kind_check CHECK (kind IN ('human', 'bot'));
ALTER TABLE users ADD CONSTRAINT users_id_kind_key UNIQUE (id, kind);

CREATE FUNCTION reject_user_kind_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.kind IS DISTINCT FROM OLD.kind THEN
        RAISE EXCEPTION 'users.kind is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'users_kind_immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER users_kind_immutable
BEFORE UPDATE OF kind ON users
FOR EACH ROW EXECUTE FUNCTION reject_user_kind_change();

-- Auth keys carry a generated human discriminator in both owner columns. The
-- composite foreign keys make it impossible to bind or stage a bot even when
-- an update bypasses the application store.
ALTER TABLE auth_keys
    ADD COLUMN auth_user_kind TEXT GENERATED ALWAYS AS ('human'::text) STORED,
    ADD COLUMN auth_pending_user_kind TEXT GENERATED ALWAYS AS ('human'::text) STORED,
    ADD CONSTRAINT auth_keys_user_kind_fkey
        FOREIGN KEY (user_id, auth_user_kind) REFERENCES users (id, kind),
    ADD CONSTRAINT auth_keys_pending_user_kind_fkey
        FOREIGN KEY (pending_user_id, auth_pending_user_kind) REFERENCES users (id, kind);

-- A bot token has one current SHA-256 digest. Rotation replaces it and
-- revocation deletes it; plaintext secrets never enter this table.
CREATE TABLE bot_tokens (
    user_id     BIGINT PRIMARY KEY,
    bot_kind    TEXT GENERATED ALWAYS AS ('bot'::text) STORED,
    secret_hash BYTEA NOT NULL CHECK (octet_length(secret_hash) = 32),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT bot_tokens_bot_kind_fkey
        FOREIGN KEY (user_id, bot_kind) REFERENCES users (id, kind) ON DELETE CASCADE
);
