-- Inert profile-photo foundations: the gallery, the current selection, the
-- upload retry receipt and the deletion-operation dedup record. Every table here
-- is empty and has no writer — no photo RPC is registered, nothing hydrates a
-- photo, and no code in internal/ touches them. What lands is the durable
-- contract the gallery slice is built on, so the ownership, current-pointer
-- and retry interlocks are provable before any writer exists that could violate
-- them. Each guarantee below is a constraint, not a convention.
--
-- Reversible: nothing reads these tables, so dropping them restores the previous
-- schema with no data loss.

-- One gallery entry per (owner, file). Rows are live and hard-deleted; dead
-- retry metadata belongs in profile_upload_receipt, never here.
--
-- PRIMARY KEY (user_id, file_id) is the gallery identity, and scanned backwards
-- it is the paging index for the gallery list, which is ordered by descending
-- file id and stays that way: reselecting an older photo cannot renumber or
-- reorder a page, so a client cursor keeps meaning the same thing.
--
-- UNIQUE (file_id) is left unnamed on purpose: its index name,
-- user_photos_file_id_key, is the access path the eraser's reverse-reference
-- probe has to plan against. The primary key leads with user_id and cannot
-- answer "does any gallery still reference this file", which is the question
-- that keeps a visible photo's bytes from being reclaimed.
--
-- UNIQUE (user_id, client_file_id) is the upload dedup key. One client file id
-- is one gallery entry per owner, so two parallel uploads of the same id cannot
-- land two rows and therefore cannot land two lifetime quota charges.
--
-- The composite foreign key to files (id, uploader_id) is the structural
-- ownership backstop: (owner 1, file 203) is unrepresentable when user 2 owns
-- 203, so no query bug in a later slice can put someone else's upload in this
-- account's gallery. RESTRICT, never CASCADE — a gallery row's lifetime is the
-- owner's decision, and an eraser must not be able to remove the row behind a
-- photo an account is still showing.
CREATE TABLE user_photos (
    user_id        BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id        BIGINT      NOT NULL,
    client_file_id BIGINT      NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, file_id),
    UNIQUE (file_id),
    UNIQUE (user_id, client_file_id),
    CONSTRAINT user_photos_file_owned_by_owner
        FOREIGN KEY (file_id, user_id) REFERENCES files (id, uploader_id)
        ON DELETE RESTRICT
);

-- Current selection, which is not the newest entry: reselecting an older
-- retained photo makes it current, and clearing the avatar promotes nothing.
-- One row per owner, created by that owner's first gallery write.
--
-- The composite foreign key is what makes "current names a live entry in that
-- owner's own gallery" structural. A pointer to a file the owner does not
-- own, or to one of their own files that is not in their gallery, cannot be
-- stored. A MATCH SIMPLE composite key leaves NULL unchecked, which is
-- exactly the empty-avatar state rather than an accident. RESTRICT is the
-- interlock that forces a writer to clear the pointer in the same transaction
-- before it deletes the gallery row the pointer names.
--
-- mutation_revision is monotonic per owner across every upload, reselection and
-- clear, and this row is never compacted: an acknowledged deletion must stay
-- acknowledged across a restore that predates the upload it cleared, and a
-- restored database counter cannot carry that on its own. The non-negative
-- check is the fail-closed half of width exhaustion.
CREATE TABLE profile_photo_state (
    user_id           BIGINT      NOT NULL PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    current_file_id   BIGINT      NULL,
    mutation_revision BIGINT      NOT NULL DEFAULT 0 CHECK (mutation_revision >= 0),
    CONSTRAINT profile_photo_state_current_is_own_gallery_entry
        FOREIGN KEY (user_id, current_file_id) REFERENCES user_photos (user_id, file_id)
        ON DELETE RESTRICT
);

-- Durable upload dedup and retry metadata, consulted before upload parts are
-- read, before a file id is allocated, and before quota is charged — a
-- uniqueness conflict at completion is too late, because the allocation
-- commits the files row before the bytes are written and the quota sums every
-- files row, stored or not.
--
-- state: 0 pending, 1 complete, 2 deleted. A receipt outlives its gallery entry.
-- After the owner deletes the photo the receipt stays as a terminal
-- record, so a retry under that client id gets the uniform unavailable-photo
-- error instead of re-uploading a deleted photo, and the dedup key is never
-- deleted together with user_photos.
--
-- file_id is nullable with ON DELETE SET NULL, and deliberately a
-- single-column foreign key: a receipt is retry metadata, not a live media
-- reference, so it must not pin a blob against reclamation. A composite key
-- would also put user_id in the SET NULL set, and user_id is half the primary
-- key. Ownership of the referenced file is the gallery row's guarantee, not the
-- receipt's.
--
-- The request fingerprint is immutable. Reusing one client id for a different
-- size, part count, payload digest or media mode is a different request and is
-- rejected rather than silently served the upload the id already bought.
-- payload_digest is SHA-256 over the assembled parts, taken when the receipt is
-- created; media_mode carries only the mode the gallery lane accepts, so an
-- unsupported mode is refused before any work rather than stored.
CREATE TABLE profile_upload_receipt (
    user_id        BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    client_file_id BIGINT      NOT NULL,
    file_id        BIGINT      NULL REFERENCES files (id) ON DELETE SET NULL,
    state          SMALLINT    NOT NULL CHECK (state IN (0, 1, 2)),
    request_size   BIGINT      NOT NULL CHECK (request_size >= 0),
    part_count     INT         NOT NULL CHECK (part_count >= 0),
    payload_digest BYTEA       NOT NULL CHECK (octet_length(payload_digest) = 32),
    media_mode     TEXT        NOT NULL CHECK (media_mode = 'photo'),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, client_file_id)
);

-- Deduplication of one deletion request, keyed by the authenticated transport
-- identity that carried it, so a retry finds the operation that already ran
-- instead of resolving "current" a second time and clearing a different photo.
-- The resolved target is bound here at first commit; a retry never re-resolves.
--
-- auth_key_id carries no foreign key on purpose. Pinning auth_keys would keep
-- revoked sessions alive, and ON DELETE CASCADE would destroy a pending
-- deletion's dedup record at the moment its session is revoked, which is exactly
-- when a retry is most likely. The owner half is still FK-backed, and the
-- auth-key id is only ever taken from the authenticated session, so a forged
-- value cannot reach this table.
--
-- target_file_id, client_file_id and clear_revision are historical identifiers,
-- not references: the gallery row they describe is deleted by this operation and
-- the files row may outlive it or be reclaimed before it. A foreign key on any
-- of them would either break on the delete or pin the file against reclamation,
-- turning an acknowledged deletion into an immediate-quota promise the design
-- does not make.
--
-- operation_key is the opaque, server-assigned identity the recovery event
-- carries. Auth-key id, session id and msg id stay in this table and never
-- travel off-alpha: they are transport identities, and the replayer does not
-- need them. Retention of these rows is bounded per owner by the writer, and the
-- primary key leads with user_id so that bound and any per-owner lookup ride the
-- same index.
CREATE TABLE profile_delete_operation (
    user_id        BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    auth_key_id    BIGINT      NOT NULL,
    session_id     BIGINT      NOT NULL,
    msg_id         BIGINT      NOT NULL,
    operation_key  BYTEA       NOT NULL CHECK (octet_length(operation_key) = 16),
    target_file_id BIGINT      NULL,
    client_file_id BIGINT      NULL,
    clear_revision BIGINT      NULL CHECK (clear_revision IS NULL OR clear_revision >= 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, auth_key_id, session_id, msg_id),
    UNIQUE (operation_key)
);
