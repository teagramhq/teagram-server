-- name: UpsertSRPChallenge :exec
INSERT INTO srp_challenges (srp_id, auth_key_id, user_id, b_secret, b_public, expires_at)
VALUES (sqlc.arg(srp_id), sqlc.arg(auth_key_id), sqlc.arg(user_id), sqlc.arg(b_secret), sqlc.arg(b_public),
        now() + (sqlc.arg(ttl_us)::bigint * interval '1 microsecond'))
ON CONFLICT (auth_key_id, user_id) DO UPDATE
SET srp_id = EXCLUDED.srp_id,
    b_secret = EXCLUDED.b_secret,
    b_public = EXCLUDED.b_public,
    expires_at = EXCLUDED.expires_at;

-- name: ConsumeSRPChallenge :one
DELETE FROM srp_challenges
WHERE srp_id = sqlc.arg(srp_id)
  AND auth_key_id = sqlc.arg(auth_key_id)
  AND expires_at > now()
RETURNING user_id, b_secret, b_public;

-- name: SweepExpiredSRPChallenges :execrows
DELETE FROM srp_challenges
WHERE srp_id IN (
    SELECT srp_id
    FROM srp_challenges
    WHERE expires_at <= now()
    ORDER BY expires_at, srp_id
    LIMIT sqlc.arg(lim)::int
    FOR UPDATE SKIP LOCKED
);
