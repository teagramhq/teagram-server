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
