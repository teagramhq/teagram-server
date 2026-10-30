#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

REPO=/opt/teagram
DEPLOYED_SHA=${1:-}
BASE_CONTAINER=teagram-teagramd-1
LOCK_PATH=/tmp/teagram-deploy.lock

if [[ "$#" != 1 ]] || ! [[ "$DEPLOYED_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  printf '%s\n' 'usage: rollback-public-websocket.sh DEPLOYED_SHA (full 40-hex SHA)' >&2
  exit 64
fi
if [[ "${OPS_SHA:-}" != "$DEPLOYED_SHA" ]]; then
  printf '%s\n' 'rollback-public-websocket: operation source SHA must match the deployed SHA' >&2
  exit 1
fi
if [[ "$(id -u)" != 0 ]]; then
  printf '%s\n' 'rollback-public-websocket: must run as root' >&2
  exit 1
fi

exec 9>"$LOCK_PATH"
flock -x 9

live_sha=$(git -C "$REPO" rev-parse HEAD)
if [[ "$live_sha" != "$DEPLOYED_SHA" ]]; then
  printf 'rollback-public-websocket: live checkout %s does not match requested SHA %s\n' \
    "$live_sha" "$DEPLOYED_SHA" >&2
  exit 1
fi

base_id=$(docker inspect --format '{{.Id}}' "$BASE_CONTAINER")
if ! [[ "$base_id" =~ ^[0-9a-f]{64}$ ]]; then
  printf '%s\n' 'rollback-public-websocket: live base container ID is invalid' >&2
  exit 1
fi
base_state=$(docker inspect --format '{{.State.Status}}' "$base_id")
if [[ "$base_state" != running ]]; then
  printf '%s\n' 'rollback-public-websocket: base teagramd is not running' >&2
  exit 1
fi
base_image=$(docker inspect --format '{{.Image}}' "$base_id")
image_sha=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$base_image")
if [[ "$image_sha" != "$DEPLOYED_SHA" ]]; then
  printf 'rollback-public-websocket: live image revision %s does not match requested SHA %s\n' \
    "$image_sha" "$DEPLOYED_SHA" >&2
  exit 1
fi
project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$base_id")
if [[ "$project" != teagram ]]; then
  printf 'rollback-public-websocket: unexpected Compose project %s\n' "$project" >&2
  exit 1
fi

removed=0
for service in public-haproxy teagramd-public-1 teagramd-public-2; do
  ids=$(docker ps --all --quiet --no-trunc \
    --filter "label=com.docker.compose.project=$project" \
    --filter "label=com.docker.compose.service=$service")
  if [[ -z "$ids" ]]; then
    continue
  fi
  mapfile -t container_ids <<< "$ids"
  for container_id in "${container_ids[@]}"; do
    if ! [[ "$container_id" =~ ^[0-9a-f]{64}$ ]]; then
      printf 'rollback-public-websocket: invalid container ID for service %s\n' "$service" >&2
      exit 1
    fi
    docker rm --force "$container_id" >/dev/null
    removed=$((removed + 1))
  done
  remaining=$(docker ps --all --quiet --no-trunc \
    --filter "label=com.docker.compose.project=$project" \
    --filter "label=com.docker.compose.service=$service")
  if [[ -n "$remaining" ]]; then
    printf 'rollback-public-websocket: service %s still has containers after removal\n' "$service" >&2
    exit 1
  fi
done

port_owners=$(docker ps --all --quiet --filter publish=2446)
if [[ -n "$port_owners" ]]; then
  printf '%s\n' 'rollback-public-websocket: a container still publishes host port 2446; refusing the code rollback' >&2
  exit 1
fi

printf 'public_websocket_rollback=ready project=%s removed_containers=%s docker_port_2446=unpublished\n' \
  "$project" "$removed"
