-- Batch reverse contact lookups without scanning the owner-oriented primary key.
-- Build concurrently so the additive index does not block contact writes.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY user_contacts_contact_owner_idx
    ON user_contacts (contact_id, owner_id);
