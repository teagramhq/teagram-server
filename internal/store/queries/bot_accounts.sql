-- name: CreateBotUser :one
INSERT INTO users (phone, login_mode, first_name, last_name, kind)
VALUES (NULL, 'username', sqlc.arg(first_name), '', 'bot')
RETURNING id;

-- name: InsertBotToken :exec
INSERT INTO bot_tokens (user_id, token_hash)
VALUES (sqlc.arg(user_id), sqlc.arg(token_hash));

-- name: ActiveBotTokenHash :one
SELECT token_hash
FROM bot_tokens
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- name: RotateBotToken :execrows
UPDATE bot_tokens
SET token_hash = sqlc.arg(token_hash),
    issued_at = clock_timestamp(),
    revoked_at = NULL
WHERE user_id = sqlc.arg(user_id);

-- name: RevokeBotToken :execrows
UPDATE bot_tokens
SET token_hash = NULL,
    revoked_at = clock_timestamp()
WHERE user_id = sqlc.arg(user_id)
  AND revoked_at IS NULL;

-- name: UserKindByID :one
SELECT kind
FROM users
WHERE id = sqlc.arg(user_id);
