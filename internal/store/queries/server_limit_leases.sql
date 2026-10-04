-- name: DeleteExpiredServerLimitLeasesForSubject :exec
DELETE FROM server_limit_leases
WHERE subject_id = $1 AND surface = $2 AND expires_at <= clock_timestamp();

-- name: CountActiveServerLimitLeases :one
SELECT count(*)::bigint AS active_count
FROM server_limit_leases
WHERE subject_id = $1 AND surface = $2 AND expires_at > clock_timestamp();

-- name: GetEarliestServerLimitLeaseExpiry :one
SELECT expires_at
FROM server_limit_leases
WHERE subject_id = $1 AND surface = $2 AND expires_at > clock_timestamp()
ORDER BY expires_at
LIMIT 1;

-- name: InsertServerLimitLease :execrows
INSERT INTO server_limit_leases (lease_id, subject_id, surface, expires_at)
VALUES ($1, $2, $3, clock_timestamp() + $4::interval)
ON CONFLICT (lease_id) DO NOTHING;

-- name: RenewServerLimitLease :execrows
UPDATE server_limit_leases
SET expires_at = clock_timestamp() + $4::interval
WHERE lease_id = $1
  AND subject_id = $2
  AND surface = $3
  AND expires_at > clock_timestamp();

-- name: DeleteServerLimitLease :execrows
DELETE FROM server_limit_leases
WHERE lease_id = $1 AND subject_id = $2 AND surface = $3;

-- name: SweepExpiredServerLimitLeases :execrows
DELETE FROM server_limit_leases
WHERE expires_at <= clock_timestamp();
