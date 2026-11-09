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
WITH targets(handle) AS (VALUES ('test1'), ('test2'))
SELECT targets.handle,
       COALESCE(
           TO_CHAR(up.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
           'NULL'
       )
FROM targets
LEFT JOIN usernames AS username
  ON username.handle = targets.handle AND username.owner_type = 'user'
LEFT JOIN user_passwords AS up
  ON up.user_id = username.owner_id
ORDER BY targets.handle;
SQL
)
if ! result=$(docker exec "$POSTGRES_CONTAINER" psql -X -A -t -q -F $'\t' \
  -U postgres -d teagram -v ON_ERROR_STOP=1 -c "$query" 2>/dev/null); then
  fail 'password timestamp query failed'
fi

mapfile -t rows <<< "$result"
timestamp_pattern='(NULL|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z)'
tab=$'\t'
[[ "${#rows[@]}" == 2 && "${rows[0]}" =~ ^test1${tab}${timestamp_pattern}$ && \
  "${rows[1]}" =~ ^test2${tab}${timestamp_pattern}$ ]] || {
  fail 'password timestamp query returned an invalid result shape'
}

printf '%s\n' "${rows[@]}"
