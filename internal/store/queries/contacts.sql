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
-- together. Mutual state probes the reverse edge per returned contact, and
-- photos retain the live gallery, stored-file, media-kind, and block checks.
WITH visible_photos AS MATERIALIZED (
    SELECT s.user_id, s.current_file_id
    FROM profile_photo_state s
    JOIN user_photos up ON up.user_id = s.user_id AND up.file_id = s.current_file_id
    JOIN files f ON f.id = up.file_id AND f.uploader_id = up.user_id
    WHERE EXISTS (
        SELECT 1
        FROM user_contacts owned
        WHERE owned.owner_id = sqlc.arg(owner_id)::BIGINT
          AND owned.contact_id = s.user_id
    )
      AND s.current_file_id IS NOT NULL
      AND f.stored = true
      AND f.media_kind = 'photo'
      AND NOT EXISTS (
          SELECT 1 FROM blocked_users bu
          WHERE bu.blocker_id = up.user_id
            AND bu.blocked_id = sqlc.arg(owner_id)::BIGINT
      )
)
SELECT uc.contact_id,
       COALESCE(reverse_contact.mutual, false)::BOOLEAN AS mutual,
       (SELECT COUNT(*)
        FROM user_contacts count_contacts
        WHERE count_contacts.owner_id = sqlc.arg(owner_id)::BIGINT) AS total,
       u.id,
       u.first_name,
       u.last_name,
       u.is_online,
       u.last_seen_at,
       un.handle AS username,
       u.kind,
       visible_photos.current_file_id AS photo_file_id
FROM user_contacts uc
LEFT JOIN LATERAL (
    SELECT true AS mutual
    FROM user_contacts reverse
    WHERE reverse.contact_id = uc.owner_id
      AND reverse.owner_id = uc.contact_id
    LIMIT 1
) AS reverse_contact ON true
JOIN users u ON u.id = uc.contact_id
LEFT JOIN usernames un ON un.owner_type = 'user' AND un.owner_id = u.id
LEFT JOIN visible_photos ON visible_photos.user_id = u.id
WHERE uc.owner_id = sqlc.arg(owner_id)::BIGINT
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
