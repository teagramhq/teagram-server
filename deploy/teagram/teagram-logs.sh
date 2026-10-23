#!/bin/sh
# Read-only teagramd log access (forced command of the teagram-qa-logs key).
# The client sends one UTC timestamp; this prints at most the last 5000 lines.
set -eu
T=${SSH_ORIGINAL_COMMAND:-}
case "$T" in
  [0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]T[0-2][0-9]:[0-5][0-9]:[0-5][0-9]Z) ;;
  *) logger -t teagram-logs "refused: '$T' client=${SSH_CLIENT:-}"; echo "teagram-logs: send one UTC time like 2026-10-17T00:00:00Z" >&2; exit 64 ;;
esac
logger -t teagram-logs "since=$T client=${SSH_CLIENT:-}"

candidate_ids=$(docker ps -q \
  --filter label=com.docker.compose.project=teagram \
  --filter label=com.docker.compose.service=teagramd)
candidate_count=$(printf '%s\n' "$candidate_ids" | wc -l | tr -d ' ')
if [ -z "$candidate_ids" ] || [ "$candidate_count" != 1 ]; then
  echo "teagram-logs: expected one running teagramd service container" >&2
  exit 1
fi

config_files=$(docker inspect \
  --format '{{ index .Config.Labels "com.docker.compose.project.config_files" }}' \
  "$candidate_ids")
if [ -z "$config_files" ]; then
  echo "teagram-logs: running teagramd container has no Compose file selection" >&2
  exit 1
fi
config_files=$(printf '%s' "$config_files" | tr ',' ':')

container_ids=$(COMPOSE_FILE="$config_files" docker compose \
  --project-directory /opt/teagram \
  --project-name teagram \
  ps -q teagramd)
container_count=$(printf '%s\n' "$container_ids" | wc -l | tr -d ' ')
if [ -z "$container_ids" ] || [ "$container_count" != 1 ]; then
  echo "teagram-logs: expected one teagramd container from the live Compose project" >&2
  exit 1
fi

docker logs --since "$T" "$container_ids" 2>&1 | tail -n 5000
