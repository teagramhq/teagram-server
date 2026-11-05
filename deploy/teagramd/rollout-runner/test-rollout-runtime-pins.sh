#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RUNNER="$SCRIPT_DIR/rollout-runner.sh"
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"

ROLLOUT_RUNNER_SOURCE_ONLY=1
source "$RUNNER"
unset ROLLOUT_RUNNER_SOURCE_ONLY

docker() {
  case "$*" in
    'compose config --services')
      [ "${MOCK_COMPOSE_SERVICES_ERROR:-0}" != 1 ] || return 42
      printf '%s\n' "${MOCK_COMPOSE_SERVICES:-teagramd}"
      ;;
    'compose config --format json')
      [ "${MOCK_COMPOSE_JSON_ERROR:-0}" != 1 ] || return 43
      printf '%s\n' "$MOCK_COMPOSE_JSON"
      ;;
    'compose up -d --no-build --no-deps --wait --wait-timeout 1 bot-api-forwarder')
      printf '%s\n' "$*" >> "$MOCK_FORWARDER_UP_EVENTS"
      [ "${MOCK_FORWARDER_UP_FAIL:-0}" != 1 ] || return 47
      ;;
    *) return 99 ;;
  esac
}

rollback_after_target_failure() {
  printf '%s\n' "$1" >> "$MOCK_ROLLBACK_EVENTS"
  [ "${MOCK_ROLLBACK_FAIL:-0}" != 1 ]
}
READY_SECONDS=1

MOCK_COMPOSE_SERVICES=$'teagramd\nbot-api-forwarder'
MOCK_COMPOSE_JSON='{"services":{"teagramd":{"environment":{"TG_BOT_API_LISTEN_ADDR":""}},"bot-api-forwarder":{}}}'
if bot_api_forwarder_health_required; then
  printf '%s\n' 'FAIL initial deployment requires the disabled Bot API forwarder to be healthy' >&2
  exit 1
else
  service_status=$?
  if [ "$service_status" = 1 ]; then
    printf '%s\n' 'PASS initial deployment skips health wait while the Bot API listener is disabled'
  else
    printf 'FAIL disabled Bot API listener check returned status %s\n' "$service_status" >&2
    exit 1
  fi
fi

MOCK_COMPOSE_JSON='{"services":{"teagramd":{"environment":{"TG_BOT_API_LISTEN_ADDR":"127.0.0.1:2447"}},"bot-api-forwarder":{}}}'
if bot_api_forwarder_health_required; then
  printf '%s\n' 'PASS enabled Bot API listener requires a healthy forwarder'
else
  printf '%s\n' 'FAIL enabled Bot API listener skips the forwarder health check' >&2
  exit 1
fi

MOCK_FORWARDER_UP_EVENTS=$(mktemp)
MOCK_ROLLBACK_EVENTS=$(mktemp)
trap 'rm -f -- "$MOCK_FORWARDER_UP_EVENTS" "$MOCK_ROLLBACK_EVENTS"' EXIT
MOCK_FORWARDER_UP_FAIL=1
if wait_for_bot_api_forwarder; then
  printf '%s\n' 'FAIL failed forwarder health was accepted' >&2
  exit 1
else
  wait_status=$?
fi
if [ "$wait_status" = 1 ] && \
   grep -Fxq 'compose up -d --no-build --no-deps --wait --wait-timeout 1 bot-api-forwarder' "$MOCK_FORWARDER_UP_EVENTS" && \
   grep -Fxq 'bot_api_forwarder_health_exit_47' "$MOCK_ROLLBACK_EVENTS"; then
  printf '%s\n' 'PASS enabled Bot API listener waits for forwarder health and rolls back on failure'
else
  printf 'FAIL enabled forwarder health wait status=%s up=%s rollback=%s\n' \
    "$wait_status" "$(cat "$MOCK_FORWARDER_UP_EVENTS")" "$(cat "$MOCK_ROLLBACK_EVENTS")" >&2
  exit 1
fi

: > "$MOCK_FORWARDER_UP_EVENTS"
: > "$MOCK_ROLLBACK_EVENTS"
MOCK_FORWARDER_UP_FAIL=0
MOCK_COMPOSE_JSON='{"services":{"teagramd":{"environment":{"TG_BOT_API_LISTEN_ADDR":""}},"bot-api-forwarder":{}}}'
if ! wait_for_bot_api_forwarder || [ -s "$MOCK_FORWARDER_UP_EVENTS" ] || [ -s "$MOCK_ROLLBACK_EVENTS" ]; then
  printf '%s\n' 'FAIL disabled Bot API listener did not skip the forwarder health wait' >&2
  exit 1
fi
rm -f -- "$MOCK_FORWARDER_UP_EVENTS" "$MOCK_ROLLBACK_EVENTS"

MOCK_COMPOSE_SERVICES=teagramd
if bot_api_forwarder_health_required; then
  printf '%s\n' 'FAIL rollout runner requires an absent Bot API forwarder' >&2
  exit 1
else
  service_status=$?
  if [ "$service_status" = 1 ]; then
    printf '%s\n' 'PASS rollout runner skips health checks when the optional forwarder is absent'
  else
    printf 'FAIL absent optional forwarder returned status %s\n' "$service_status" >&2
    exit 1
  fi
fi

MOCK_COMPOSE_SERVICES_ERROR=1
if bot_api_forwarder_health_required; then
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

MOCK_COMPOSE_SERVICES_ERROR=0
MOCK_COMPOSE_SERVICES=$'teagramd\nbot-api-forwarder'
MOCK_COMPOSE_JSON_ERROR=1
if bot_api_forwarder_health_required; then
  printf '%s\n' 'FAIL rollout runner accepts an unavailable Compose configuration' >&2
  exit 1
else
  service_status=$?
  if [ "$service_status" = 2 ]; then
    printf '%s\n' 'PASS rollout runner propagates Compose configuration errors'
  else
    printf 'FAIL Compose configuration returned status %s\n' "$service_status" >&2
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
