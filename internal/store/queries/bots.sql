-- name: CreateBotUser :one
INSERT INTO users (phone, login_mode, kind, first_name, last_name)
VALUES (NULL, 'username', 'bot', sqlc.arg(first_name), '')
RETURNING id, phone, first_name, last_name, created_at, is_online, last_seen_at;

-- name: BotIDByUsername :one
SELECT u.id
FROM users u
JOIN usernames un ON un.owner_type = 'user' AND un.owner_id = u.id
WHERE u.kind = 'bot' AND un.handle = lower(sqlc.arg(handle));

-- name: UpsertBotToken :exec
INSERT INTO bot_tokens (user_id, secret_hash)
VALUES (sqlc.arg(user_id), sqlc.arg(secret_hash))
ON CONFLICT (user_id) DO UPDATE
SET secret_hash = EXCLUDED.secret_hash,
    created_at = clock_timestamp();

-- name: ActiveBotTokenHashByID :one
SELECT secret_hash
FROM bot_tokens
WHERE user_id = sqlc.arg(user_id);

-- name: DeleteBotToken :execrows
DELETE FROM bot_tokens
WHERE user_id = sqlc.arg(user_id);
