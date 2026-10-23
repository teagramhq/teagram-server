#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/teagram-logs.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$DOCKER_CALLS"
case "$1" in
  ps)
    [[ "$*" == 'ps -q --filter label=com.docker.compose.project=teagram --filter label=com.docker.compose.service=teagramd' ]]
    if [[ "${FIXTURE_NO_CANDIDATE:-}" != 1 ]]; then
      printf 'fixture-candidate\n'
    fi
    ;;
  inspect)
    [[ "$2" == --format && "$4" == fixture-candidate ]]
    printf '%s\n' "$FIXTURE_COMPOSE_FILES"
    ;;
  compose)
    [[ "$*" == 'compose --project-directory /opt/teagram --project-name teagram ps -q teagramd' ]]
    [[ "$COMPOSE_FILE" == "$FIXTURE_COMPOSE_FILE" ]] || {
      printf 'unexpected COMPOSE_FILE: %s\n' "$COMPOSE_FILE" >&2
      exit 91
    }
    if [[ "${FIXTURE_NO_COMPOSE_ID:-}" != 1 ]]; then
      printf 'fixture-compose-container\n'
    fi
    ;;
  logs)
    [[ "$2" == --since && "${4:-}" == fixture-compose-container ]] || {
      printf 'docker logs received the wrong container: %s\n' "$*"
      exit 92
    }
    for ((line = 1; line <= 5002; line++)); do
      printf 'line-%s\n' "$line"
    done
    ;;
  *)
    printf 'unexpected docker call: %s\n' "$*" >&2
    exit 99
    ;;
esac
DOCKER
cat > "$tmp/bin/logger" <<'LOGGER'
#!/bin/sh
exit 0
LOGGER
chmod 755 "$tmp/bin/docker" "$tmp/bin/logger"

export PATH="$tmp/bin:$PATH"
export DOCKER_CALLS="$tmp/docker-calls"
export FIXTURE_COMPOSE_FILES='/opt/teagram/docker-compose.yml,/opt/teagram/docker-compose.local-blobs.yml,/opt/teagram/docker-compose.override.yml'
export FIXTURE_COMPOSE_FILE='/opt/teagram/docker-compose.yml:/opt/teagram/docker-compose.local-blobs.yml:/opt/teagram/docker-compose.override.yml'

output=$(SSH_ORIGINAL_COMMAND=2026-10-23T03:40:00Z "$script")
[[ "$(printf '%s\n' "$output" | wc -l | tr -d ' ')" == 5000 ]]
[[ "$output" != *line-1$'\n'* ]]
[[ "$output" == *line-3$'\n'* ]]
[[ "$output" == *line-5002 ]]
grep -Fqx 'compose --project-directory /opt/teagram --project-name teagram ps -q teagramd' "$DOCKER_CALLS"
grep -Fqx 'logs --since 2026-10-23T03:40:00Z fixture-compose-container' "$DOCKER_CALLS"
if grep -Fq 'telegram-server-telegramd-1' "$DOCKER_CALLS"; then
  echo 'FAIL log command used the retired fixed container name' >&2
  exit 1
fi

: > "$DOCKER_CALLS"
status=0
SSH_ORIGINAL_COMMAND='docker ps' "$script" >"$tmp/invalid.out" 2>"$tmp/invalid.err" || status=$?
[[ "$status" == 64 ]]
[[ ! -s "$DOCKER_CALLS" ]]
grep -Fq 'teagram-logs: send one UTC time' "$tmp/invalid.err"

: > "$DOCKER_CALLS"
status=0
FIXTURE_NO_CANDIDATE=1 SSH_ORIGINAL_COMMAND=2026-10-23T03:40:00Z \
  "$script" >"$tmp/missing.out" 2>"$tmp/missing.err" || status=$?
[[ "$status" == 1 ]]
grep -Fq 'expected one running teagramd service container' "$tmp/missing.err"
if grep -Fq 'compose ' "$DOCKER_CALLS"; then
  echo 'FAIL Compose was called without a live service container' >&2
  exit 1
fi

: > "$DOCKER_CALLS"
status=0
FIXTURE_NO_COMPOSE_ID=1 SSH_ORIGINAL_COMMAND=2026-10-23T03:40:00Z \
  "$script" >"$tmp/no-compose-id.out" 2>"$tmp/no-compose-id.err" || status=$?
[[ "$status" == 1 ]]
grep -Fq 'expected one teagramd container from the live Compose project' "$tmp/no-compose-id.err"
if grep -Fq 'logs ' "$DOCKER_CALLS"; then
  echo 'FAIL logs were requested without a resolved Compose container' >&2
  exit 1
fi

echo 'teagram-logs fixture cases pass'
