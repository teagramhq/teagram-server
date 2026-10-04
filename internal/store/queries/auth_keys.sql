-- name: SaveAuthKey :exec
INSERT INTO auth_keys (id, key_value)
VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE
  SET key_value = EXCLUDED.key_value,
      last_seen_at = now();

-- name: AuthKeyByID :one
SELECT ak.id, ak.key_value, ak.user_id, ak.created_at, ak.last_seen_at, ak.pending_user_id,
       ak.pending_started_at,
       u.login_mode,
       (up.user_id IS NOT NULL) AS has_password
FROM auth_keys ak
LEFT JOIN users u ON u.id = ak.user_id
LEFT JOIN user_passwords up ON up.user_id = ak.user_id
WHERE ak.id = $1;

-- name: BindAuthKeyUser :execrows
UPDATE auth_keys SET user_id = $2, pending_user_id = NULL, pending_started_at = NULL WHERE id = $1;

-- name: LockUnboundAuthKey :one
-- Locks the key admission will bind, rejecting keys already authorized. A
-- pending password challenge is cleared by the same transaction when signup
-- binds the still-unbound key.
SELECT id FROM auth_keys
WHERE id = $1 AND user_id IS NULL
FOR UPDATE;

-- name: TouchAuthKey :exec
UPDATE auth_keys SET last_seen_at = now() WHERE id = $1;

-- name: DeleteAuthKey :exec
DELETE FROM auth_keys WHERE id = $1;

-- name: AuthKeysByUser :many
SELECT * FROM auth_keys WHERE user_id = $1;

-- name: SetPendingUser :one
UPDATE auth_keys
SET user_id = NULL, pending_user_id = sqlc.arg(user_id), pending_started_at = clock_timestamp()
WHERE id = sqlc.arg(id)
RETURNING pending_started_at;

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
SET user_id = sqlc.arg(user_id), pending_user_id = NULL, pending_started_at = NULL
WHERE id = sqlc.arg(id)
  AND pending_user_id = sqlc.arg(user_id)
  AND pending_started_at = sqlc.arg(started_at)::timestamptz
  AND pending_started_at IS NOT NULL
  AND pending_started_at + (sqlc.arg(lifetime_micros)::bigint * interval '1 microsecond') > clock_timestamp();
