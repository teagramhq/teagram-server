#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RUNNER="$SCRIPT_DIR/rollout-runner.sh"
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"

ROLLOUT_RUNNER_SOURCE_ONLY=1
source "$RUNNER"
unset ROLLOUT_RUNNER_SOURCE_ONLY

docker() {
  [ "$*" = 'compose config --services' ] || return 99
  [ "${MOCK_COMPOSE_SERVICES_ERROR:-0}" != 1 ] || return 42
  printf '%s\n' "${MOCK_COMPOSE_SERVICES:-teagramd}"
}

MOCK_COMPOSE_SERVICES=$'teagramd\nbot-api-forwarder'
if compose_has_service bot-api-forwarder; then
  printf '%s\n' 'PASS rollout runner detects the optional Bot API forwarder service'
else
  printf '%s\n' 'FAIL rollout runner misses the optional Bot API forwarder service' >&2
  exit 1
fi

MOCK_COMPOSE_SERVICES=teagramd
if compose_has_service bot-api-forwarder; then
  printf '%s\n' 'FAIL rollout runner treats an absent Bot API forwarder as present' >&2
  exit 1
else
  service_status=$?
  if [ "$service_status" = 1 ]; then
    printf '%s\n' 'PASS rollout runner distinguishes an absent optional forwarder'
  else
    printf 'FAIL absent optional forwarder returned status %s\n' "$service_status" >&2
    exit 1
  fi
fi

MOCK_COMPOSE_SERVICES_ERROR=1
if compose_has_service bot-api-forwarder; then
  printf '%s\n' 'FAIL rollout runner accepts an unavailable Compose service list' >&2
  exit 1
else
  service_status=$?
  if [ "$service_status" = 2 ]; then
    printf '%s\n' 'PASS rollout runner propagates Compose service discovery errors'
  else
    printf 'FAIL Compose service discovery returned status %s\n' "$service_status" >&2
    exit 1
  fi
fi

approved_verifier_sha=$(sed -n 's/^readonly APPROVED_VERIFIER_SHA=//p' "$RUNNER")
actual_verifier_sha=$(sha256sum "$VERIFIER" | awk '{print $1}')

if [ "$approved_verifier_sha" = "$actual_verifier_sha" ]; then
  printf '%s\n' 'PASS rollout runner approves the current verifier artifact'
else
  printf 'FAIL rollout verifier pin: approved=%s actual=%s\n' \
    "$approved_verifier_sha" "$actual_verifier_sha" >&2
  exit 1
fi
