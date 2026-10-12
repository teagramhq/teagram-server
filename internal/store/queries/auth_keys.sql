-- name: SaveAuthKey :exec
INSERT INTO auth_keys (id, key_value)
VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE
  SET key_value = EXCLUDED.key_value,
      last_seen_at = now();

-- name: AuthKeyByID :one
SELECT ak.id, ak.key_value, ak.user_id, ak.created_at, ak.last_seen_at, ak.pending_user_id,
       ak.pending_started_at,
       GREATEST(
           0::bigint,
           COALESCE(
               FLOOR(EXTRACT(EPOCH FROM (
                   ak.pending_started_at
                   + (sqlc.arg(pending_lifetime_micros)::bigint * interval '1 microsecond')
                   - clock_timestamp()
               )) * 1000000)::bigint,
               0::bigint
           )
       )::bigint AS pending_remaining_micros,
       u.login_mode,
       (up.user_id IS NOT NULL) AS has_password
FROM auth_keys ak
LEFT JOIN users u ON u.id = ak.user_id
LEFT JOIN user_passwords up ON up.user_id = ak.user_id
WHERE ak.id = sqlc.arg(id);

-- name: BindAuthKeyUser :execrows
UPDATE auth_keys
SET user_id = $2, pending_user_id = NULL, pending_started_at = NULL,
    device_model = NULL, platform = NULL, system_version = NULL,
    api_id = NULL, app_version = NULL, ip = NULL
WHERE id = $1;

-- name: BindAuthKeyUserWithMetadata :execrows
UPDATE auth_keys
SET user_id = sqlc.arg(user_id), pending_user_id = NULL, pending_started_at = NULL,
    device_model = sqlc.arg(device_model), platform = sqlc.arg(platform),
    system_version = sqlc.arg(system_version), api_id = sqlc.arg(api_id),
    app_version = sqlc.arg(app_version), ip = sqlc.arg(ip)
WHERE id = sqlc.arg(id);

-- name: LockUnboundAuthKey :one
-- Locks the key admission will bind, rejecting keys already authorized. A
-- pending password challenge is cleared by the same transaction when signup
-- binds the still-unbound key.
SELECT id FROM auth_keys
WHERE id = $1 AND user_id IS NULL
FOR UPDATE;

-- name: TouchAuthKey :exec
UPDATE auth_keys SET last_seen_at = now() WHERE id = $1;

-- name: TouchAuthKeyWithIP :exec
UPDATE auth_keys
SET last_seen_at = now(),
    ip = CASE
        WHEN user_id IS NULL THEN NULL
        WHEN user_id = NULLIF(sqlc.arg(expected_user_id)::bigint, 0) THEN sqlc.arg(ip)
        ELSE ip
    END,
    device_model = CASE
        WHEN user_id = NULLIF(sqlc.arg(expected_user_id)::bigint, 0)
            AND sqlc.arg(update_metadata)::boolean
            AND device_model IS DISTINCT FROM sqlc.arg(device_model)::text
        THEN sqlc.arg(device_model)::text ELSE device_model
    END,
    platform = CASE
        WHEN user_id = NULLIF(sqlc.arg(expected_user_id)::bigint, 0)
            AND sqlc.arg(update_metadata)::boolean
            AND platform IS DISTINCT FROM sqlc.arg(platform)::text
        THEN sqlc.arg(platform)::text ELSE platform
    END,
    system_version = CASE
        WHEN user_id = NULLIF(sqlc.arg(expected_user_id)::bigint, 0)
            AND sqlc.arg(update_metadata)::boolean
            AND system_version IS DISTINCT FROM sqlc.arg(system_version)::text
        THEN sqlc.arg(system_version)::text ELSE system_version
    END,
    api_id = CASE
        WHEN user_id = NULLIF(sqlc.arg(expected_user_id)::bigint, 0)
            AND sqlc.arg(update_metadata)::boolean
            AND api_id IS DISTINCT FROM sqlc.arg(api_id)::integer
        THEN sqlc.arg(api_id)::integer ELSE api_id
    END,
    app_version = CASE
        WHEN user_id = NULLIF(sqlc.arg(expected_user_id)::bigint, 0)
            AND sqlc.arg(update_metadata)::boolean
            AND app_version IS DISTINCT FROM sqlc.arg(app_version)::text
        THEN sqlc.arg(app_version)::text ELSE app_version
    END
WHERE id = sqlc.arg(id);

-- name: DeleteAuthKey :exec
DELETE FROM auth_keys WHERE id = $1;

-- name: ResetAuthorization :execrows
UPDATE auth_keys
SET user_id = NULL, pending_user_id = NULL, pending_started_at = NULL,
    device_model = NULL, platform = NULL, system_version = NULL,
    api_id = NULL, app_version = NULL, ip = NULL
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(owner_id);

-- name: UnbindAuthKeyForLogout :one
WITH target AS MATERIALIZED (
    SELECT auth_key.id, auth_key.user_id
    FROM auth_keys AS auth_key
    WHERE auth_key.id = sqlc.arg(id)
    FOR UPDATE
), unbound AS (
    UPDATE auth_keys AS key
    SET user_id = NULL, pending_user_id = NULL, pending_started_at = NULL,
        device_model = NULL, platform = NULL, system_version = NULL,
        api_id = NULL, app_version = NULL, ip = NULL
    FROM target
    WHERE key.id = target.id
    RETURNING target.user_id
)
SELECT COALESCE(unbound.user_id, 0)::bigint AS user_id
FROM unbound;

-- name: AuthKeysByUser :many
SELECT * FROM auth_keys WHERE user_id = $1;

-- name: SetAuthorizationTTLDays :execrows
UPDATE users
SET authorization_ttl_days = sqlc.arg(authorization_ttl_days)
WHERE id = sqlc.arg(user_id);

-- name: AuthorizationTTLDaysByUser :one
SELECT authorization_ttl_days
FROM users
WHERE id = sqlc.arg(user_id);

-- name: TouchBoundAuthKey :execrows
UPDATE auth_keys
SET last_seen_at = now()
WHERE id = sqlc.arg(auth_key_id)
  AND user_id = sqlc.arg(user_id);

-- name: SweepExpiredAuthorizationBatch :many
WITH expired AS MATERIALIZED (
    SELECT ak.id, ak.user_id
    FROM auth_keys ak
    JOIN users u ON u.id = ak.user_id
    -- Keep the partial-index predicate explicit for the ordered batch scan.
    WHERE ak.user_id IS NOT NULL
      AND ak.last_seen_at < now() - (u.authorization_ttl_days * interval '1 day')
      AND NOT (
          u.login_mode = 'username'
          AND NOT EXISTS (SELECT 1 FROM user_passwords up WHERE up.user_id = u.id)
      )
    ORDER BY ak.last_seen_at, ak.id
    LIMIT sqlc.arg(batch)::int
    FOR UPDATE OF ak SKIP LOCKED
), unbound AS (
    UPDATE auth_keys AS ak
    SET user_id = NULL, pending_user_id = NULL, pending_started_at = NULL,
        device_model = NULL, platform = NULL, system_version = NULL,
        api_id = NULL, app_version = NULL, ip = NULL
    FROM expired
    WHERE ak.id = expired.id
      AND ak.user_id = expired.user_id
    RETURNING expired.id, expired.user_id
)
SELECT id, user_id
FROM unbound;

-- name: LockAuthKeyForResetAuthorization :one
SELECT id FROM auth_keys
WHERE id = sqlc.arg(caller_id)
  AND user_id = sqlc.arg(owner_id)
FOR UPDATE;

-- name: UnbindOtherAuthKeysForOwner :many
-- Lock and return the old user binding while a promotion waiting on a row lock
-- is rechecked against its committed state.
WITH targets AS MATERIALIZED (
    SELECT auth_key.id, auth_key.user_id
    FROM auth_keys AS auth_key
    WHERE auth_key.id <> sqlc.arg(caller_id)
      AND (auth_key.user_id = sqlc.arg(owner_id) OR auth_key.pending_user_id = sqlc.arg(owner_id))
    FOR UPDATE
), unbound AS (
    UPDATE auth_keys AS key
    SET user_id = NULL, pending_user_id = NULL, pending_started_at = NULL,
        device_model = NULL, platform = NULL, system_version = NULL,
        api_id = NULL, app_version = NULL, ip = NULL
    FROM targets
    WHERE key.id = targets.id
      AND (key.user_id = sqlc.arg(owner_id) OR key.pending_user_id = sqlc.arg(owner_id))
    RETURNING targets.id, targets.user_id
)
SELECT id, user_id
FROM unbound;

-- name: SetPendingUser :one
UPDATE auth_keys
SET user_id = NULL, pending_user_id = sqlc.arg(user_id), pending_started_at = clock_timestamp(),
    device_model = NULL, platform = NULL, system_version = NULL,
    api_id = NULL, app_version = NULL, ip = NULL
WHERE id = sqlc.arg(id)
RETURNING pending_started_at,
       GREATEST(
           0::bigint,
           FLOOR(EXTRACT(EPOCH FROM (
               pending_started_at
               + (sqlc.arg(lifetime_micros)::bigint * interval '1 microsecond')
               - clock_timestamp()
           )) * 1000000)::bigint
       )::bigint AS pending_remaining_micros;

-- name: PendingLoginByID :one
SELECT pending_user_id, pending_started_at,
       COALESCE(
           pending_started_at + (sqlc.arg(lifetime_micros)::bigint * interval '1 microsecond') > clock_timestamp(),
           false
       ) AS active
FROM auth_keys
WHERE id = sqlc.arg(id) AND pending_user_id IS NOT NULL;

-- name: ClearExpiredPendingUser :execrows
UPDATE auth_keys
SET pending_user_id = NULL, pending_started_at = NULL
WHERE id = sqlc.arg(id)
  AND pending_user_id = sqlc.arg(user_id)
  AND pending_started_at IS NOT DISTINCT FROM sqlc.narg(started_at)::timestamptz
  AND (
      pending_started_at IS NULL
      OR pending_started_at + (sqlc.arg(lifetime_micros)::bigint * interval '1 microsecond') <= clock_timestamp()
  );

-- name: PromotePendingUser :execrows
UPDATE auth_keys
SET user_id = sqlc.arg(user_id), pending_user_id = NULL, pending_started_at = NULL,
    device_model = sqlc.arg(device_model), platform = sqlc.arg(platform),
    system_version = sqlc.arg(system_version), api_id = sqlc.arg(api_id),
    app_version = sqlc.arg(app_version), ip = sqlc.arg(ip)
WHERE id = sqlc.arg(id)
  AND pending_user_id = sqlc.arg(user_id)
  AND pending_started_at = sqlc.arg(started_at)::timestamptz
  AND pending_started_at IS NOT NULL
  AND pending_started_at + (sqlc.arg(lifetime_micros)::bigint * interval '1 microsecond') > clock_timestamp();

-- name: LockAuthKeyForPromotion :one
SELECT id
FROM auth_keys
WHERE id = sqlc.arg(id)
FOR UPDATE;
