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

MOCK_ROLLBACK_EVENTS=$(mktemp)
ROLLBACK_DIR=$(mktemp -d)
trap 'rm -f -- "$MOCK_ROLLBACK_EVENTS"; rm -rf -- "$ROLLBACK_DIR"' EXIT
PREVIOUS_SHA=9999999999999999999999999999999999999999
BASE_IMAGE_ID=sha256:0000000000000000000000000000000000000000000000000000000000000000
ROLLOUT_COMPOSE_SERVICE=teagramd
ADDED_ENV=''
CHANGED_ENV=''
MOCK_ROLLBACK_ORPHAN_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
MOCK_ROLLBACK_TEAGRAMD_ID=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

restore_checkout_and_tag() { printf '%s\n' restore_checkout >> "$MOCK_ROLLBACK_EVENTS"; }
validate_rollback_blob_authority() { return 0; }
restore_removed_public_profile_services() { return 0; }
wait_for_restored_public_profile_health() { return 0; }
current_service_id() { printf '%s\n' "$MOCK_ROLLBACK_TEAGRAMD_ID"; }
capture_snapshot() { printf 'capture_snapshot %s\n' "$3" >> "$MOCK_ROLLBACK_EVENTS"; }
compare_rollback_to_baseline() { return 0; }
run_rollback_readiness() { return 0; }
record_verified_env() { return 0; }
write_immutable() { return 0; }

docker() {
  case "$*" in
    'compose up -d --no-build --no-deps --wait --wait-timeout 1 postgres'|\
    'compose up -d --no-build --no-deps teagramd'|\
    'compose up -d --no-build --no-deps --force-recreate --wait --wait-timeout 1 bot-api-forwarder'|\
    'rm --force aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa')
      printf 'docker %s\n' "$*" >> "$MOCK_ROLLBACK_EVENTS"
      ;;
    'compose config --services')
      printf 'docker %s\n' "$*" >> "$MOCK_ROLLBACK_EVENTS"
      printf '%s\n' "$MOCK_ROLLBACK_SERVICES"
      ;;
    'inspect bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb --format {{ index .Config.Labels "com.docker.compose.project" }}')
      printf 'docker %s\n' "$*" >> "$MOCK_ROLLBACK_EVENTS"
      printf '%s\n' fixture
      ;;
    'ps --all --quiet --no-trunc --filter label=com.docker.compose.project=fixture --filter label=com.docker.compose.service=bot-api-forwarder')
      printf 'docker %s\n' "$*" >> "$MOCK_ROLLBACK_EVENTS"
      printf '%s\n' "$MOCK_ROLLBACK_ORPHAN_ID"
      ;;
    *)
      printf 'unexpected rollback docker call: %s\n' "$*" >&2
      return 98
      ;;
  esac
}

MOCK_ROLLBACK_SERVICES=$'teagramd\nbot-api-forwarder'
: > "$MOCK_ROLLBACK_EVENTS"
if ! perform_rollback >/dev/null; then
  printf '%s\n' 'FAIL rollback fixture with the forwarder enabled must succeed' >&2
  exit 1
fi
teagramd_up_line=$(grep -nFx 'docker compose up -d --no-build --no-deps teagramd' "$MOCK_ROLLBACK_EVENTS" | cut -d: -f1 || true)
forwarder_up_line=$(grep -nFx 'docker compose up -d --no-build --no-deps --force-recreate --wait --wait-timeout 1 bot-api-forwarder' "$MOCK_ROLLBACK_EVENTS" | cut -d: -f1 || true)
if [ -n "$teagramd_up_line" ] && [ -n "$forwarder_up_line" ] && \
   [ "$teagramd_up_line" -lt "$forwarder_up_line" ]; then
  printf '%s\n' 'PASS rollback recreates the enabled Bot API forwarder after teagramd'
else
  printf '%s\n' 'FAIL rollback must recreate the enabled Bot API forwarder after teagramd' >&2
  cat "$MOCK_ROLLBACK_EVENTS" >&2
  exit 1
fi

MOCK_ROLLBACK_SERVICES=teagramd
: > "$MOCK_ROLLBACK_EVENTS"
if ! perform_rollback >/dev/null; then
  printf '%s\n' 'FAIL rollback fixture without the forwarder must succeed' >&2
  exit 1
fi
teagramd_up_line=$(grep -nFx 'docker compose up -d --no-build --no-deps teagramd' "$MOCK_ROLLBACK_EVENTS" | cut -d: -f1 || true)
orphan_rm_line=$(grep -nFx "docker rm --force $MOCK_ROLLBACK_ORPHAN_ID" "$MOCK_ROLLBACK_EVENTS" | cut -d: -f1 || true)
if [ -n "$teagramd_up_line" ] && [ -n "$orphan_rm_line" ] && \
   [ "$teagramd_up_line" -lt "$orphan_rm_line" ]; then
  printf '%s\n' 'PASS rollback removes an orphaned Bot API forwarder when baseline Compose omits it'
else
  printf '%s\n' 'FAIL rollback must remove the orphaned Bot API forwarder when baseline Compose omits it' >&2
  cat "$MOCK_ROLLBACK_EVENTS" >&2
  exit 1
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
