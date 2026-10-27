-- name: ContactExists :one
SELECT EXISTS (
    SELECT 1 FROM user_contacts
    WHERE owner_id = $1 AND contact_id = $2
);

-- name: CountContacts :one
SELECT COUNT(*) FROM user_contacts WHERE owner_id = $1;

-- name: CountContactsForImport :one
SELECT COUNT(*) AS contact_count,
       EXISTS (
           SELECT 1
           FROM user_privacy_rules privacy
           WHERE privacy.owner_id = $1
             AND privacy.privacy_key = 'status_timestamp'
             AND privacy.base_rule = 'contacts'
       ) AS last_seen_contacts_rule
FROM user_contacts contacts
WHERE contacts.owner_id = $1;

-- name: InsertContact :execrows
INSERT INTO user_contacts (owner_id, contact_id)
VALUES ($1, $2)
ON CONFLICT (owner_id, contact_id) DO NOTHING;

-- name: DeleteContact :execrows
DELETE FROM user_contacts
WHERE owner_id = $1 AND contact_id = $2;

-- name: DeleteContacts :execrows
DELETE FROM user_contacts
WHERE owner_id = sqlc.arg(owner_id)
  AND contact_id = ANY(sqlc.arg(contact_ids)::bigint[]);

-- name: ListContacts :many
SELECT uc.contact_id,
       EXISTS (
           SELECT 1 FROM user_contacts reverse
           WHERE reverse.owner_id = uc.contact_id
             AND reverse.contact_id = uc.owner_id
       ) AS mutual,
       COUNT(*) OVER () AS total
FROM user_contacts uc
WHERE uc.owner_id = $1
ORDER BY uc.contact_id;

-- name: ContactStates :many
-- Use a join for the reciprocal edge so the bounded selected contacts drive
-- primary-key probes instead of a hashed scan of the owner's full contact set.
SELECT uc.contact_id,
       COALESCE(reverse.owner_id IS NOT NULL, false)::boolean AS mutual
FROM user_contacts AS uc
LEFT JOIN user_contacts AS reverse
  ON reverse.owner_id = uc.contact_id
 AND reverse.contact_id = uc.owner_id
WHERE uc.owner_id = $1
  AND uc.contact_id = ANY(sqlc.arg(contact_ids)::bigint[])
ORDER BY uc.contact_id;
