#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
wait_script="$repo_root/.github/scripts/wait-for-main-priority-controller.sh"
test_tmp="$(mktemp -d)"
trap 'rm -rf "$test_tmp"' EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

gh() {
  [[ "$#" == 4 && "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/commits/${MAIN_HEAD_SHA}/statuses" && "$3" == --jq ]] || return 2
  [[ "$4" == "[.[] | select(.context == \"main-priority-${MAIN_RUN_ID}-${MAIN_RUN_ATTEMPT}\")][0].state // \"\"" ]] || return 2

  local count=0
  [[ ! -f "$STATUS_CALLS_FILE" ]] || count="$(<"$STATUS_CALLS_FILE")"
  count=$((count + 1))
  printf '%s\n' "$count" > "$STATUS_CALLS_FILE"
  case "$STATUS_MODE" in
    success)
      if (( count == 1 )); then printf 'pending\n'; else printf 'success\n'; fi
      ;;
    failure) printf 'failure\n' ;;
    timeout) printf 'pending\n' ;;
    api-error) return 2 ;;
    *) return 2 ;;
  esac
}
sleep() { :; }
export -f gh sleep

common_env=(
  GITHUB_REPOSITORY=example/repo
  MAIN_RUN_ID=900
  MAIN_RUN_ATTEMPT=2
  MAIN_HEAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  PRIORITY_MAX_STATUS_CHECKS=2
  PRIORITY_STATUS_CHECK_INTERVAL_SECONDS=0
  STATUS_CALLS_FILE="$test_tmp/status-calls"
)
env "${common_env[@]}" STATUS_MODE=success bash "$wait_script" > "$test_tmp/success-output" ||
  fail 'main priority must proceed after the trusted controller posts success'
[[ "$(<"$test_tmp/status-calls")" == 2 ]] || fail 'the main gate must wait through a pending trusted-controller status'

for mode in failure timeout api-error; do
  rm -f "$test_tmp/status-calls"
  env "${common_env[@]}" STATUS_MODE="$mode" bash "$wait_script" > "$test_tmp/$mode-output" ||
    fail "main priority must continue when trusted-controller status is $mode"
  [[ "$(<"$test_tmp/$mode-output")" == *'::warning::'* && "$(<"$test_tmp/$mode-output")" == *'Main CI fan-out will proceed without confirmed PR cancellation'* ]] ||
    fail "main priority must warn before continuing when trusted-controller status is $mode"
done

rm -f "$test_tmp/status-calls"
env \
  GITHUB_REPOSITORY=example/repo \
  MAIN_RUN_ID=900 \
  MAIN_RUN_ATTEMPT=2 \
  MAIN_HEAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  PRIORITY_STATUS_CHECK_INTERVAL_SECONDS=0 \
  STATUS_CALLS_FILE="$test_tmp/default-timeout-calls" \
  STATUS_MODE=timeout \
  bash "$wait_script" > "$test_tmp/default-timeout-output" ||
  fail 'the default main priority wait must continue after its bounded timeout'
[[ "$(<"$test_tmp/default-timeout-calls")" == 31 ]] || fail 'the main priority wait must default to a five-minute bound'

printf 'main priority controller gate tests pass\n'
