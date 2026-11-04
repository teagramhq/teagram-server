-- name: EvaluateLastSeenPrivacy :many
-- Resolve the rule inputs for every viewer in one database round trip. The
-- contacts edge is deliberately owner -> viewer, including for reciprocity.
SELECT requested.viewer_id::BIGINT AS viewer_id,
       COALESCE(owner_rule.base_rule, 'everybody') AS owner_base_rule,
       EXISTS (
           SELECT 1 FROM blocked_users bu
           WHERE bu.blocker_id = sqlc.arg(owner_id)
             AND bu.blocked_id = requested.viewer_id
       ) AS owner_blocked,
       EXISTS (
           SELECT 1 FROM user_privacy_rule_users pru
           WHERE pru.owner_id = sqlc.arg(owner_id)
             AND pru.privacy_key = 'status_timestamp'
             AND pru.user_id = requested.viewer_id
             AND pru.entry_type = 'disallow'
       ) AS owner_disallowed,
       EXISTS (
           SELECT 1 FROM user_privacy_rule_users pru
           WHERE pru.owner_id = sqlc.arg(owner_id)
             AND pru.privacy_key = 'status_timestamp'
             AND pru.user_id = requested.viewer_id
             AND pru.entry_type = 'allow'
       ) AS owner_allowed,
       EXISTS (
           SELECT 1 FROM user_contacts uc
           WHERE uc.owner_id = sqlc.arg(owner_id)
             AND uc.contact_id = requested.viewer_id
       ) AS owner_contact,
       COALESCE((
           EXISTS (
               SELECT 1 FROM blocked_users bu
               WHERE bu.blocker_id = requested.viewer_id
                 AND bu.blocked_id = sqlc.arg(owner_id)
           )
           OR EXISTS (
               SELECT 1 FROM user_privacy_rules viewer_rule
               WHERE viewer_rule.owner_id = requested.viewer_id
                 AND viewer_rule.privacy_key = 'status_timestamp'
                 AND (
                     EXISTS (
                         SELECT 1 FROM user_privacy_rule_users pru
                         WHERE pru.owner_id = requested.viewer_id
                           AND pru.privacy_key = 'status_timestamp'
                           AND pru.user_id = sqlc.arg(owner_id)
                           AND pru.entry_type = 'disallow'
                     )
                     OR (
                         NOT EXISTS (
                             SELECT 1 FROM user_privacy_rule_users pru
                             WHERE pru.owner_id = requested.viewer_id
                               AND pru.privacy_key = 'status_timestamp'
                               AND pru.user_id = sqlc.arg(owner_id)
                               AND pru.entry_type = 'allow'
                         )
                         AND (
                             viewer_rule.base_rule = 'nobody'
                             OR (
                                 viewer_rule.base_rule = 'contacts'
                                 AND NOT EXISTS (
                                     SELECT 1 FROM user_contacts uc
                                     WHERE uc.owner_id = requested.viewer_id
                                       AND uc.contact_id = sqlc.arg(owner_id)
                                 )
                             )
                         )
                     )
                 )
           )
       ), FALSE)::BOOLEAN AS viewer_hides_from_owner
FROM UNNEST(sqlc.arg(viewer_ids)::BIGINT[]) AS requested(viewer_id)
JOIN users owner_user
  ON owner_user.id = sqlc.arg(owner_id)
JOIN users viewer_user
  ON viewer_user.id = requested.viewer_id
LEFT JOIN user_privacy_rules owner_rule
  ON owner_rule.owner_id = owner_user.id
 AND owner_rule.privacy_key = 'status_timestamp';

-- name: EvaluateLastSeenPrivacyForViewer :many
-- Resolve every owner's status visibility to one viewer in a single query.
-- Each edge is a left join on its primary key, so every owner resolves to one
-- row and the planner can hash the set-valued sides instead of running a
-- subplan per owner.
WITH owner_users AS MATERIALIZED (
    -- Look up only the requested primary keys so a large user table is never
    -- scanned; the missing-owner check stays in the join.
    SELECT users.id
    FROM users
    WHERE users.id = ANY (sqlc.arg(owner_ids)::BIGINT[])
),
viewer_rule AS MATERIALIZED (
    SELECT rule.base_rule
    FROM user_privacy_rules rule
    WHERE rule.owner_id = sqlc.arg(viewer_id)
      AND rule.privacy_key = 'status_timestamp'
)
SELECT requested.owner_id::BIGINT AS owner_id,
       COALESCE(owner_rule.base_rule, 'everybody') AS owner_base_rule,
       (owner_blocks_viewer.blocked_id IS NOT NULL)::BOOLEAN AS owner_blocked,
       (owner_disallows_viewer.user_id IS NOT NULL)::BOOLEAN AS owner_disallowed,
       (owner_allows_viewer.user_id IS NOT NULL)::BOOLEAN AS owner_allowed,
       (owner_contact_of_viewer.contact_id IS NOT NULL)::BOOLEAN AS owner_contact,
       (
           viewer_blocks_owner.blocked_id IS NOT NULL
           OR (
               viewer_rule.base_rule IS NOT NULL
               AND (
                   viewer_disallows_owner.user_id IS NOT NULL
                   OR (
                       viewer_allows_owner.user_id IS NULL
                       AND (
                           viewer_rule.base_rule = 'nobody'
                           OR (
                               viewer_rule.base_rule = 'contacts'
                               AND viewer_contact_of_owner.contact_id IS NULL
                           )
                       )
                   )
               )
           )
       )::BOOLEAN AS viewer_hides_from_owner
FROM UNNEST(sqlc.arg(owner_ids)::BIGINT[]) AS requested(owner_id)
JOIN owner_users owner_user
  ON owner_user.id = requested.owner_id
JOIN users viewer_user
  ON viewer_user.id = sqlc.arg(viewer_id)
LEFT JOIN user_privacy_rules owner_rule
  ON owner_rule.owner_id = owner_user.id
 AND owner_rule.privacy_key = 'status_timestamp'
LEFT JOIN blocked_users owner_blocks_viewer
  ON owner_blocks_viewer.blocker_id = owner_user.id
 AND owner_blocks_viewer.blocked_id = sqlc.arg(viewer_id)
LEFT JOIN user_privacy_rule_users owner_disallows_viewer
  ON owner_disallows_viewer.owner_id = owner_user.id
 AND owner_disallows_viewer.privacy_key = 'status_timestamp'
 AND owner_disallows_viewer.user_id = sqlc.arg(viewer_id)
 AND owner_disallows_viewer.entry_type = 'disallow'
LEFT JOIN user_privacy_rule_users owner_allows_viewer
  ON owner_allows_viewer.owner_id = owner_user.id
 AND owner_allows_viewer.privacy_key = 'status_timestamp'
 AND owner_allows_viewer.user_id = sqlc.arg(viewer_id)
 AND owner_allows_viewer.entry_type = 'allow'
LEFT JOIN user_contacts owner_contact_of_viewer
  ON owner_contact_of_viewer.owner_id = owner_user.id
 AND owner_contact_of_viewer.contact_id = sqlc.arg(viewer_id)
LEFT JOIN blocked_users viewer_blocks_owner
  ON viewer_blocks_owner.blocker_id = sqlc.arg(viewer_id)
 AND viewer_blocks_owner.blocked_id = owner_user.id
LEFT JOIN viewer_rule ON TRUE
LEFT JOIN user_privacy_rule_users viewer_disallows_owner
  ON viewer_disallows_owner.owner_id = sqlc.arg(viewer_id)
 AND viewer_disallows_owner.privacy_key = 'status_timestamp'
 AND viewer_disallows_owner.user_id = owner_user.id
 AND viewer_disallows_owner.entry_type = 'disallow'
LEFT JOIN user_privacy_rule_users viewer_allows_owner
  ON viewer_allows_owner.owner_id = sqlc.arg(viewer_id)
 AND viewer_allows_owner.privacy_key = 'status_timestamp'
 AND viewer_allows_owner.user_id = owner_user.id
 AND viewer_allows_owner.entry_type = 'allow'
LEFT JOIN user_contacts viewer_contact_of_owner
  ON viewer_contact_of_owner.owner_id = sqlc.arg(viewer_id)
 AND viewer_contact_of_owner.contact_id = owner_user.id;

-- name: GetLastSeenPrivacyRule :one
SELECT base_rule
FROM user_privacy_rules
WHERE owner_id = $1 AND privacy_key = 'status_timestamp';

-- name: GetLastSeenPrivacyRuleUsers :many
SELECT user_id, entry_type
FROM user_privacy_rule_users
WHERE owner_id = $1 AND privacy_key = 'status_timestamp'
ORDER BY user_id, entry_type;

-- name: UpsertLastSeenPrivacyRule :exec
INSERT INTO user_privacy_rules (owner_id, privacy_key, base_rule)
VALUES ($1, 'status_timestamp', $2)
ON CONFLICT (owner_id, privacy_key) DO UPDATE SET base_rule = EXCLUDED.base_rule;

-- name: DeleteLastSeenPrivacyRuleUsers :exec
DELETE FROM user_privacy_rule_users
WHERE owner_id = $1 AND privacy_key = 'status_timestamp';

-- name: InsertLastSeenPrivacyAllowedUsers :exec
INSERT INTO user_privacy_rule_users (owner_id, privacy_key, user_id, entry_type)
SELECT $1, 'status_timestamp', requested.user_id, 'allow'
FROM UNNEST($2::BIGINT[]) AS requested(user_id);

-- name: InsertLastSeenPrivacyDisallowedUsers :exec
INSERT INTO user_privacy_rule_users (owner_id, privacy_key, user_id, entry_type)
SELECT $1, 'status_timestamp', requested.user_id, 'disallow'
FROM UNNEST($2::BIGINT[]) AS requested(user_id);
