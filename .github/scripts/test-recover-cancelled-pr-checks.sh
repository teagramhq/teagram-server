#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
recovery_script="$repo_root/.github/scripts/recover-cancelled-pr-checks.sh"
test_tmp="$(mktemp -d)"
trap 'rm -rf "$test_tmp"' EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

gh() {
  if [[ "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/actions/runs/900" ]]; then
    [[ "$3" == --jq && "$4" == '.run_started_at // ""' ]] || return 2
    printf '2026-10-28T00:00:00Z\n'
    return 0
  fi
  if [[ "$1" == api && "$2" == --paginate && "$3" == --method && "$4" == GET ]]; then
    local workflow_path="$5"
    case "$workflow_path" in
      "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs")
        printf '101\tcompleted\tcancelled\t%s\t1\t2026-10-28T00:00:00Z\n' "${CI_HEAD_SHA:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
        return 0
        ;;
      "repos/${GITHUB_REPOSITORY}/actions/workflows/perf.yml/runs")
        printf '202\tcompleted\tcancelled\t%s\t2\t2026-10-28T00:01:00Z\n' "${PERF_HEAD_SHA:-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}"
        return 0
        ;;
      "repos/${GITHUB_REPOSITORY}/actions/workflows/real-client-browser-flow.yml/runs")
        printf '303\tcompleted\tcancelled\t%s\t3\t2026-10-28T00:02:00Z\n' "${BROWSER_HEAD_SHA:-cccccccccccccccccccccccccccccccccccccccc}"
        return 0
        ;;
      *) return 2 ;;
    esac
  fi
  if [[ "$1" == api && "$2" == --paginate && "$3" == "repos/${GITHUB_REPOSITORY}/actions/runs/"*"/jobs?per_page=100" ]]; then
    if [[ "$RECOVERY_MODE" == failed-ci && "$3" == *'/101/jobs?'* ]]; then
      printf '1\n'
    else
      printf '0\n'
    fi
    return 0
  fi
  if [[ "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/pulls/"* ]]; then
    local pr_number="${2##*/}"
    case "$pr_number" in
      1) printf 'open\t%s\n' "${CI_HEAD_SHA:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" ;;
      2) printf 'open\t%s\n' "${CURRENT_PERF_HEAD_SHA:-${PERF_HEAD_SHA:-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}}" ;;
      3) printf 'open\t%s\n' "${BROWSER_HEAD_SHA:-cccccccccccccccccccccccccccccccccccccccc}" ;;
      *) return 2 ;;
    esac
    return 0
  fi
  if [[ "$1" == run && "$2" == rerun ]]; then
    printf '%s\n' "$3" >> "$RERUN_LOG"
    return 0
  fi
  return 2
}
sleep() { :; }
export -f gh sleep

common_env=(
  GITHUB_REPOSITORY=example/repo
  MAIN_RUN_ID=900
  ISSUE=MAIN-3126
  RECOVERY_MODE=success
  RERUN_LOG="$test_tmp/reruns"
  CANCELLED_PR_RUN_TARGETS='ci.yml:101:2026-10-28T00:00:00Z perf.yml:202:2026-10-28T00:01:00Z'
)
env "${common_env[@]}" bash "$recovery_script" > "$test_tmp/recovery-output" || fail 'recovery must process cancelled checks across all three workflows'
[[ "$(<"$test_tmp/reruns")" == $'101\n202\n303' ]] || fail 'recovery must re-run pre-main cancellations and discover PR checks cancelled after main started'

rm "$test_tmp/reruns"
env "${common_env[@]}" RECOVERY_MODE=failed-ci CURRENT_PERF_HEAD_SHA=dddddddddddddddddddddddddddddddddddddddd bash "$recovery_script" > "$test_tmp/safety-output" || fail 'recovery should safely skip failed or stale PR checks'
[[ "$(<"$test_tmp/reruns")" == '303' ]] || fail 'recovery must skip a cancelled CI run with failed work and a performance run for a stale head'
[[ "$(<"$test_tmp/safety-output")" == *'router already handled fail-fast'* && "$(<"$test_tmp/safety-output")" == *'not the cancelled run head'* ]] || fail 'recovery must explain skipped failed and stale runs'

printf 'cancelled PR check recovery tests pass\n'
