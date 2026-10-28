-- Retire an issued row whose expiry has passed so the handle can be issued
-- again. The clock is PostgreSQL's clock, not a process timestamp.
-- name: ExpireRegistrationInvite :execrows
UPDATE registration_invites
SET state = 'expired'
WHERE handle = lower(sqlc.arg(handle))
  AND state = 'issued'
  AND expires_at <= clock_timestamp();

-- Issue returns metadata but never returns the stored digest. The secret is
-- generated and retained only by the caller that receives the result.
-- name: InsertRegistrationInvite :one
WITH issued AS (SELECT clock_timestamp() AS at)
INSERT INTO registration_invites (handle, secret_digest, issued_at, expires_at)
SELECT
    lower(sqlc.arg(handle)),
    sqlc.arg(secret_digest),
    issued.at,
    issued.at + (sqlc.arg(lifetime_us)::bigint * interval '1 microsecond')
FROM issued
RETURNING id, handle, inviter_user_id, issued_at, expires_at, state, consumed_at, revoked_at;

-- User-issued invites are not tied to a username. Their inviter is recorded on
-- the invite row and the secret digest is the only credential persisted.
-- name: InsertUserRegistrationInvite :one
WITH issued AS (SELECT clock_timestamp() AS at)
INSERT INTO registration_invites (handle, inviter_user_id, secret_digest, issued_at, expires_at)
SELECT
    NULL,
    sqlc.arg(inviter_user_id),
    sqlc.arg(secret_digest),
    issued.at,
    issued.at + (sqlc.arg(lifetime_us)::bigint * interval '1 microsecond')
FROM issued
RETURNING id, handle, inviter_user_id, issued_at, expires_at, state, consumed_at, revoked_at;

-- The digest lookup is the single locked invite lookup for verification and
-- redemption. The handle probe runs in the same query for every request so
-- absent, terminal, and valid digests use the same database path; user invites
-- cannot claim a handle reserved by a live operator invite.
-- name: RegistrationInviteBySecretDigestForUpdate :one
WITH operator_handle AS MATERIALIZED (
    SELECT EXISTS (
        SELECT 1
        FROM registration_invites candidate
        WHERE candidate.handle = lower(sqlc.arg(handle))
          AND candidate.state = 'issued'
          AND candidate.expires_at > clock_timestamp()
          AND candidate.inviter_user_id IS NULL
    ) AS reserved
), locked AS MATERIALIZED (
    SELECT id, handle, secret_digest, inviter_user_id, issued_at, expires_at,
           state, consumed_at, revoked_at, consumed_by_user_id
    FROM registration_invites invite
    WHERE invite.secret_digest = sqlc.arg(secret_digest)
    FOR UPDATE
)
SELECT locked.id, locked.handle, locked.secret_digest, locked.inviter_user_id,
       locked.issued_at, locked.expires_at, locked.state, locked.consumed_at,
       locked.revoked_at, locked.consumed_by_user_id,
       true AS found,
       COALESCE(locked.state = 'issued' AND locked.expires_at > clock_timestamp(), false)::boolean AS live,
       operator_handle.reserved AS operator_handle_reserved
FROM locked CROSS JOIN operator_handle
UNION ALL
SELECT 0::bigint,
       NULL::text,
       decode(repeat('00', 64), 'hex')::bytea,
       NULL::bigint,
       NULL::timestamptz,
       NULL::timestamptz,
       'expired'::text,
       NULL::timestamptz,
       NULL::timestamptz,
       NULL::bigint,
       false,
       false,
       operator_handle.reserved
FROM operator_handle
WHERE NOT EXISTS (SELECT 1 FROM locked);

-- Guard the state transition as well as taking the row lock above: if expiry
-- passes while the caller is comparing the secret, the update refuses.
-- name: ConsumeRegistrationInvite :execrows
UPDATE registration_invites
SET state = 'consumed', consumed_at = clock_timestamp(),
    consumed_by_user_id = sqlc.narg(consumed_by_user_id)
WHERE id = sqlc.arg(id)
  AND secret_digest = sqlc.arg(secret_digest)
  AND state = 'issued'
  AND expires_at > clock_timestamp();

-- Revoke and consume both update the same row. PostgreSQL's row-lock order
-- therefore decides the race: whichever update acquires the lock first wins.
-- name: LockRegistrationInvite :one
SELECT id
FROM registration_invites
WHERE id = sqlc.arg(id)
FOR UPDATE;

-- name: RevokeRegistrationInvite :execrows
UPDATE registration_invites
SET state = 'revoked', revoked_at = clock_timestamp()
WHERE id = sqlc.arg(id)
  AND state = 'issued'
  AND expires_at > clock_timestamp();

-- State is projected from database time so a naturally expired issued row is
-- reported as expired even before another writer retires it.
-- name: ListRegistrationInvites :many
SELECT id, handle, inviter_user_id, issued_at, expires_at,
       CASE
           WHEN state = 'issued' AND expires_at <= clock_timestamp() THEN 'expired'
           ELSE state
       END::text AS state,
       consumed_at, revoked_at
FROM registration_invites
ORDER BY id;
