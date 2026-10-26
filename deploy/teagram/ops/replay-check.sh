#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO=/opt/teagram
TARGET_SHA=${1:-}

if [[ "$#" != 1 ]] || ! [[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  printf '%s\n' 'usage: replay-check.sh TARGET_SHA (full 40-hex SHA)' >&2
  exit 64
fi
if ! git -C "$REPO" merge-base --is-ancestor "$TARGET_SHA" origin/main; then
  printf 'replay-check: %s is not on origin/main\n' "$TARGET_SHA" >&2
  exit 65
fi
if ! git -C "$REPO" cat-file -e "$TARGET_SHA:deploy/teagramd/rollout-runner/rollout-verifier.sh" 2>/dev/null; then
  printf 'replay-check: runner verifier is missing at %s\n' "$TARGET_SHA" >&2
  exit 66
fi

if [[ ! -d /tmp || -L /tmp || "$(stat -c '%u:%a' /tmp)" != 0:1777 ]]; then
  printf '%s\n' 'replay-check: /tmp is not a root-owned mode-1777 directory' >&2
  exit 1
fi

CONTAINER=teagram-teagramd-1
CONTAINER_ID=$(docker inspect --format '{{.Id}}' "$CONTAINER")
if ! [[ "$CONTAINER_ID" =~ ^[0-9a-f]{64}$ ]]; then
  printf '%s\n' 'replay-check: live teagramd container ID is invalid' >&2
  exit 1
fi
IMAGE_ID=$(docker inspect --format '{{.Image}}' "$CONTAINER")
LIVE_SHA=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$IMAGE_ID")
if [[ "$LIVE_SHA" != "$TARGET_SHA" ]]; then
  printf 'replay-check: live image revision %s does not match target %s\n' "$LIVE_SHA" "$TARGET_SHA" >&2
  exit 1
fi

LIVE_COMPOSE=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$CONTAINER" | tr ',' ':')
S3_ENV_FILE=
case "$LIVE_COMPOSE" in
  "$REPO"/.rollout-compose.local-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f].yml:"$REPO"/docker-compose.override.yml) ;;
  "$REPO"/docker-compose.yml:"$REPO"/docker-compose.local-blobs.yml:"$REPO"/docker-compose.override.yml) ;;
  "$REPO"/docker-compose.yml:"$REPO"/docker-compose.override.yml)
    for candidate in \
      "$REPO/.env.rustfs-parked" \
      "$(dirname "$REPO")/telegram-server/.env.rustfs-parked" \
      /root/.env.rustfs-parked; do
      if [[ -e "$candidate" || -L "$candidate" ]]; then
        if [[ ! -f "$candidate" || -L "$candidate" || -n "$S3_ENV_FILE" ]]; then
          printf '%s\n' 'replay-check: parked Compose environment file is invalid or ambiguous' >&2
          exit 1
        fi
        S3_ENV_FILE=$candidate
      fi
    done
    if [[ -z "$S3_ENV_FILE" ]]; then
      printf '%s\n' 'replay-check: parked Compose environment file is unavailable' >&2
      exit 1
    fi
    export COMPOSE_ENV_FILES="$REPO/.env,$S3_ENV_FILE"
    ;;
  *) printf 'replay-check: live Compose selection is not approved: %s\n' "$LIVE_COMPOSE" >&2; exit 1 ;;
esac
export COMPOSE_FILE=$LIVE_COMPOSE

EVIDENCE=$(mktemp -d "/tmp/teagram-replay-check.${TARGET_SHA:0:12}.XXXXXX")
chmod 0700 "$EVIDENCE"
RUNNER="$EVIDENCE/rollout-runner.sh"
VERIFIER="$EVIDENCE/rollout-verifier.sh"
if ! git -C "$REPO" show "$TARGET_SHA:deploy/teagramd/rollout-runner/rollout-runner.sh" > "$RUNNER"; then
  printf '%s\n' 'replay-check: could not extract the target rollout runner' >&2
  printf 'result=failed target_sha=%s reason=runner-unavailable\n' "$TARGET_SHA" > "$EVIDENCE/replay-check.result"
  exit 1
fi
if ! git -C "$REPO" show "$TARGET_SHA:deploy/teagramd/rollout-runner/rollout-verifier.sh" > "$VERIFIER"; then
  printf '%s\n' 'replay-check: could not extract the target runner verifier' >&2
  printf 'result=failed target_sha=%s reason=verifier-unavailable\n' "$TARGET_SHA" > "$EVIDENCE/replay-check.result"
  exit 1
fi
chmod 0700 "$RUNNER"
chmod 0700 "$VERIFIER"
APPROVED_VERIFIER_SHA=$(sed -n 's/^readonly APPROVED_VERIFIER_SHA=//p' "$RUNNER")
ACTUAL_VERIFIER_SHA=$(sha256sum "$VERIFIER" | cut -d' ' -f1)
if ! [[ "$APPROVED_VERIFIER_SHA" =~ ^[0-9a-f]{64}$ ]] || \
  [[ "$APPROVED_VERIFIER_SHA" != "$ACTUAL_VERIFIER_SHA" ]]; then
  printf 'replay-check: target verifier pin mismatch at %s\n' "$TARGET_SHA" >&2
  printf 'result=failed target_sha=%s reason=verifier-pin-mismatch evidence=%s\n' "$TARGET_SHA" "$EVIDENCE" > "$EVIDENCE/replay-check.result"
  exit 1
fi
if ! bash -n "$VERIFIER"; then
  printf 'replay-check: runner verifier at %s failed bash -n\n' "$TARGET_SHA" >&2
  printf 'result=failed target_sha=%s reason=invalid-verifier evidence=%s\n' "$TARGET_SHA" "$EVIDENCE" > "$EVIDENCE/replay-check.result"
  exit 1
fi
printf 'target_sha=%s\nlive_sha=%s\ncontainer_id=%s\ncompose_files=%s\nverifier_sha=%s\n' \
  "$TARGET_SHA" "$LIVE_SHA" "$CONTAINER_ID" "$LIVE_COMPOSE" "$ACTUAL_VERIFIER_SHA" > "$EVIDENCE/replay-check.txt"

if (cd "$REPO" && ROLLOUT_CHECKOUT_PATH="$REPO" bash "$VERIFIER" ready target "$CONTAINER_ID" "$EVIDENCE"); then
  printf 'result=pass target_sha=%s container_id=%s evidence=%s\n' \
    "$TARGET_SHA" "$CONTAINER_ID" "$EVIDENCE" > "$EVIDENCE/replay-check.result"
  printf 'replay_check=pass target_sha=%s evidence=%s\n' "$TARGET_SHA" "$EVIDENCE"
else
  rc=$?
  printf 'result=failed exit=%s target_sha=%s container_id=%s evidence=%s\n' \
    "$rc" "$TARGET_SHA" "$CONTAINER_ID" "$EVIDENCE" > "$EVIDENCE/replay-check.result"
  printf 'replay_check=failed exit=%s target_sha=%s evidence=%s\n' "$rc" "$TARGET_SHA" "$EVIDENCE" >&2
  exit "$rc"
fi
