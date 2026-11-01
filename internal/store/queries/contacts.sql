-- name: ContactExists :one
SELECT EXISTS (
    SELECT 1 FROM user_contacts
    WHERE owner_id = $1 AND contact_id = $2
);

-- name: CountContacts :one
SELECT COUNT(*) FROM user_contacts WHERE owner_id = $1;

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

-- name: ContactsWithUsers :many
-- Load the full contact page, user records, and visible current photos
-- together. The materialized contact list also feeds the result directly, so
-- the owner's edges are not scanned twice. Mutual state stays bounded to the
-- viewer's contact IDs; photos retain the gallery, storage, media-kind, and
-- block checks. Store restores contact-ID order without sorting the wide row.
WITH owned_contacts AS MATERIALIZED (
    SELECT contact_id
    FROM user_contacts
    WHERE owner_id = sqlc.arg(owner_id)::BIGINT
),
mutual_contacts AS MATERIALIZED (
    SELECT reverse.owner_id AS contact_id
    FROM user_contacts reverse
    WHERE reverse.contact_id = sqlc.arg(owner_id)::BIGINT
      AND reverse.owner_id = ANY (
          ARRAY(SELECT contact_id FROM owned_contacts)
      )
),
visible_photos AS MATERIALIZED (
    SELECT s.user_id, s.current_file_id
    FROM owned_contacts owned
    JOIN profile_photo_state s ON s.user_id = owned.contact_id
    JOIN user_photos up ON up.user_id = s.user_id AND up.file_id = s.current_file_id
    JOIN files f ON f.id = up.file_id AND f.uploader_id = up.user_id
    WHERE s.current_file_id IS NOT NULL
      AND f.stored = true
      AND f.media_kind = 'photo'
      AND NOT EXISTS (
          SELECT 1 FROM blocked_users bu
          WHERE bu.blocker_id = up.user_id
            AND bu.blocked_id = sqlc.arg(owner_id)::BIGINT
      )
)
SELECT uc.contact_id,
       (mutual_contacts.contact_id IS NOT NULL)::BOOLEAN AS mutual,
       (SELECT COUNT(*) FROM owned_contacts) AS total,
       u.first_name,
       u.last_name,
       u.is_online,
       u.last_seen_at,
       un.handle AS username,
       u.kind,
       visible_photos.current_file_id AS photo_file_id
FROM owned_contacts uc
JOIN users u ON u.id = uc.contact_id
LEFT JOIN mutual_contacts ON mutual_contacts.contact_id = uc.contact_id
LEFT JOIN usernames un ON un.owner_type = 'user' AND un.owner_id = u.id
LEFT JOIN visible_photos ON visible_photos.user_id = u.id;

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
