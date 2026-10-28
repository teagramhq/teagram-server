-- Expand registration invites to include user-issued, handle-less rows.
ALTER TABLE registration_invites
    ALTER COLUMN handle DROP NOT NULL;

ALTER TABLE registration_invites
    ADD COLUMN inviter_user_id BIGINT NULL REFERENCES users(id),
    ADD COLUMN consumed_by_user_id BIGINT NULL REFERENCES users(id);

ALTER TABLE registration_invites
    ADD CONSTRAINT registration_invites_handle_inviter_kind_check
    CHECK (
        (handle IS NOT NULL AND handle <> '' AND inviter_user_id IS NULL)
        OR (handle IS NULL AND inviter_user_id IS NOT NULL)
    );

CREATE UNIQUE INDEX registration_invites_secret_digest_idx
    ON registration_invites (secret_digest);
