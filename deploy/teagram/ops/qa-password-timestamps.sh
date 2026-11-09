#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram
readonly SERVER_CONTAINER=teagram-teagramd-1
readonly POSTGRES_CONTAINER=teagram-postgres-1
readonly EXPECTED_COMPOSE_FILES="$REPO/docker-compose.yml,$REPO/docker-compose.override.yml"

fail() {
  printf 'qa-password-timestamps: %s\n' "$1" >&2
  exit 1
}

if [[ "$#" != 0 ]]; then
  printf '%s\n' 'usage: qa-password-timestamps.sh' >&2
  exit 64
fi
[[ "$(id -u)" == 0 ]] || fail 'root is required'
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || fail 'reviewed source SHA is invalid'

inspect_container() {
  local name=$1 expected_service=$2 inspection state project service compose_files
  inspection=$(docker inspect --format '{{.State.Status}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$name" 2>/dev/null) || {
    fail "cannot inspect the existing $expected_service container"
  }
  IFS='|' read -r state project service compose_files <<< "$inspection"
  [[ "$state" == running && "$project" == teagram && "$service" == "$expected_service" && \
    "$compose_files" == "$EXPECTED_COMPOSE_FILES" ]] || {
    fail "the existing $expected_service container is not in the reviewed live stack"
  }
}

inspect_container "$SERVER_CONTAINER" teagramd
inspect_container "$POSTGRES_CONTAINER" postgres

if ! docker exec "$POSTGRES_CONTAINER" pg_isready --quiet --username postgres --dbname teagram >/dev/null 2>&1; then
  fail 'Postgres is not ready'
fi

query=$(cat <<'SQL'
WITH targets(handle) AS (VALUES ('qa'), ('qa2'), ('test1'), ('test2')),
username_rows AS (
    SELECT targets.handle,
           username.handle AS username_handle,
           username.owner_type,
           username.owner_id
    FROM targets
    LEFT JOIN usernames AS username ON username.handle = targets.handle
),
user_links AS (
    SELECT username_rows.handle, users.id AS user_id
    FROM username_rows
    JOIN users ON username_rows.owner_type = 'user' AND users.id = username_rows.owner_id

    UNION

    SELECT targets.handle, users.id
    FROM targets
    JOIN users ON users.username = targets.handle
),
user_summary AS (
    SELECT targets.handle,
           COUNT(user_links.user_id) AS user_count,
           MIN(user_links.user_id) AS user_id
    FROM targets
    LEFT JOIN user_links ON user_links.handle = targets.handle
    GROUP BY targets.handle
)
SELECT targets.handle,
       CASE
           WHEN user_summary.user_count = 1 THEN 'PRESENT'
           WHEN user_summary.user_count > 1 THEN 'MULTIPLE'
           WHEN username_rows.username_handle IS NULL THEN 'UNRESOLVED_BY_HANDLE'
           WHEN username_rows.owner_type = 'user' THEN 'MISSING_TARGET_USER'
           ELSE 'NOT_A_USER_HANDLE'
       END,
       CASE WHEN username_rows.username_handle IS NULL THEN 'ABSENT' ELSE 'PRESENT' END,
       CASE
           WHEN username_rows.username_handle IS NULL THEN 'NONE'
           WHEN username_rows.owner_type = 'user' AND username_owner.id IS NOT NULL THEN 'YES'
           ELSE 'NO'
       END,
       'NOT_STORED',
       CASE
           WHEN user_summary.user_count = 1 THEN COALESCE(
               TO_CHAR(up.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
               'NULL'
           )
           WHEN user_summary.user_count > 1 THEN 'AMBIGUOUS'
           ELSE 'UNKNOWN'
       END
FROM targets
JOIN username_rows ON username_rows.handle = targets.handle
JOIN user_summary ON user_summary.handle = targets.handle
LEFT JOIN users AS username_owner
  ON username_rows.owner_type = 'user' AND username_owner.id = username_rows.owner_id
LEFT JOIN user_passwords AS up
  ON user_summary.user_count = 1 AND up.user_id = user_summary.user_id
ORDER BY targets.handle;
SQL
)
if ! result=$(docker exec "$POSTGRES_CONTAINER" psql -X -A -t -q -F $'\t' \
  -U postgres -d teagram -v ON_ERROR_STOP=1 -c "$query"); then
  fail 'account status query failed'
fi

mapfile -t rows <<< "$result"
user_row_pattern='(PRESENT|MULTIPLE|MISSING_TARGET_USER|NOT_A_USER_HANDLE|UNRESOLVED_BY_HANDLE)'
username_row_pattern='(PRESENT|ABSENT)'
username_owner_pattern='(YES|NO|NONE)'
timestamp_pattern='(UNKNOWN|AMBIGUOUS|NULL|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z)'
tab=$'\t'
if [[ "${#rows[@]}" == 4 && \
  "${rows[0]}" =~ ^qa${tab}${user_row_pattern}${tab}${username_row_pattern}${tab}${username_owner_pattern}${tab}NOT_STORED${tab}${timestamp_pattern}$ && \
  "${rows[1]}" =~ ^qa2${tab}${user_row_pattern}${tab}${username_row_pattern}${tab}${username_owner_pattern}${tab}NOT_STORED${tab}${timestamp_pattern}$ && \
  "${rows[2]}" =~ ^test1${tab}${user_row_pattern}${tab}${username_row_pattern}${tab}${username_owner_pattern}${tab}NOT_STORED${tab}${timestamp_pattern}$ && \
  "${rows[3]}" =~ ^test2${tab}${user_row_pattern}${tab}${username_row_pattern}${tab}${username_owner_pattern}${tab}NOT_STORED${tab}${timestamp_pattern}$ ]]; then
  :
else
  fail 'account status query returned an invalid result shape'
fi

printf '%s\n' "${rows[@]}"
