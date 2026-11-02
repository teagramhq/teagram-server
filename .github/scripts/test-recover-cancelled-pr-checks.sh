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
  local path run_id run_count run_status run_conclusion run_attempt run_head_sha run_pr_number run_created_at

  if [[ "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/actions/runs/900" ]]; then
    [[ "$3" == --jq && "$4" == '.run_started_at // ""' ]] || return 2
    printf '2026-10-28T00:00:00Z\n'
    return 0
  fi
  if [[ "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=100" ]]; then
    if [[ "$RECOVERY_MODE" == cancel-once ]] && [[ ! -e "$MAIN_ACTIVE_SEEN" ]] && (( $(rerun_count 101) >= 1 )); then
      : > "$MAIN_ACTIVE_SEEN"
      printf '900\n'
    fi
    return 0
  fi
  if [[ "$1" == api && "$2" == --paginate && "$3" == "repos/${GITHUB_REPOSITORY}/actions/runs/900/jobs?per_page=100" ]]; then
    printf 'in_progress\n'
    return 0
  fi
  if [[ "$1" == api && "$2" == --paginate && "$3" == --method && "$4" == GET ]]; then
    path="$5"
    case "$path" in
      "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs")
        run_id=101
        run_head_sha="${CI_HEAD_SHA:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}"
        run_pr_number=1
        run_created_at=2026-10-28T00:00:00Z
        ;;
      "repos/${GITHUB_REPOSITORY}/actions/workflows/perf.yml/runs")
        run_id=202
        run_head_sha="${PERF_HEAD_SHA:-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}"
        run_pr_number=2
        run_created_at=2026-10-28T00:01:00Z
        ;;
      "repos/${GITHUB_REPOSITORY}/actions/workflows/real-client-browser-flow.yml/runs")
        run_id=303
        run_head_sha="${BROWSER_HEAD_SHA:-cccccccccccccccccccccccccccccccccccccccc}"
        run_pr_number=3
        run_created_at=2026-10-28T00:02:00Z
        ;;
      *) return 2 ;;
    esac
    [[ "$RECOVERY_MODE" != no-target ]] || return 0
    run_count="$(rerun_count "$run_id")"
    run_attempt=1
    run_status=completed
    run_conclusion=cancelled
    case "$RECOVERY_MODE" in
      rerun-failure)
        ;;
      exhausted)
        if (( run_count > 0 )); then
          run_attempt=2
          run_status=in_progress
          run_conclusion=none
        fi
        ;;
      cancel-once)
        if (( run_count == 1 )); then
          run_attempt=2
        elif (( run_count > 1 )); then
          run_attempt=3
          run_conclusion=success
        fi
        ;;
      *)
        if (( run_count > 0 )); then
          run_attempt=2
          run_conclusion=success
        fi
        ;;
    esac
    if [[ "$RECOVERY_MODE" == failed-ci && "$run_id" == 101 ]]; then
      run_attempt=1
      run_status=completed
      run_conclusion=cancelled
    fi
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$run_id" "$run_status" "$run_conclusion" "$run_head_sha" "$run_pr_number" "$run_created_at" "$run_attempt"
    return 0
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
    [[ "$RECOVERY_MODE" != rerun-failure ]] || return 2
    return 0
  fi
  return 2
}
rerun_count() {
  [[ -f "$RERUN_LOG" ]] || { printf '0\n'; return; }
  awk -v id="$1" '$0 == id { count++ } END { print count + 0 }' "$RERUN_LOG"
}
sleep() { :; }
export -f gh rerun_count sleep

common_env=(
  GITHUB_REPOSITORY=example/repo
  MAIN_RUN_ID=900
  ISSUE=MAIN-3126
  RECOVERY_MODE=success
  RERUN_LOG="$test_tmp/reruns"
  MAIN_ACTIVE_SEEN="$test_tmp/main-active-seen"
  RECOVERY_MAX_STATUS_CHECKS=10
  CANCELLED_PR_RUN_TARGETS='ci.yml:101:2026-10-28T00:00:00Z perf.yml:202:2026-10-28T00:01:00Z'
)
env "${common_env[@]}" bash "$recovery_script" > "$test_tmp/recovery-output" || fail 'recovery must process cancelled checks across all three workflows'
[[ "$(<"$test_tmp/reruns")" == $'101\n202\n303' ]] || fail 'recovery must re-run pre-main cancellations and discover PR checks cancelled after main started'

rm "$test_tmp/reruns"
env "${common_env[@]}" RECOVERY_MODE=failed-ci CURRENT_PERF_HEAD_SHA=dddddddddddddddddddddddddddddddddddddddd bash "$recovery_script" > "$test_tmp/safety-output" || fail 'recovery should safely skip failed or stale PR checks'
[[ "$(<"$test_tmp/reruns")" == '303' ]] || fail 'recovery must skip a cancelled CI run with failed work and a performance run for a stale head'
[[ "$(<"$test_tmp/safety-output")" == *'router already handled fail-fast'* && "$(<"$test_tmp/safety-output")" == *'not the cancelled run head'* ]] || fail 'recovery must explain skipped failed and stale runs'

rm "$test_tmp/reruns" "$test_tmp/main-active-seen" 2>/dev/null || true
env "${common_env[@]}" RECOVERY_MODE=cancel-once RECOVERY_MAX_STATUS_CHECKS=10 bash "$recovery_script" > "$test_tmp/cancel-once-output" || fail 'recovery must retry runs cancelled again while main CI is active'
[[ "$(<"$test_tmp/reruns")" == $'101\n303\n101\n202\n303\n202' ]] || fail 'recovery must retry cancelled reruns after main CI becomes idle'
[[ "$(<"$test_tmp/cancel-once-output")" == *'Deferring rerun of perf.yml run 202 until main CI run 900 completes'* ]] || fail 'recovery must wait when another main aggregate is active'

rm "$test_tmp/reruns"
if env "${common_env[@]}" RECOVERY_MODE=rerun-failure RECOVERY_MAX_STATUS_CHECKS=2 bash "$recovery_script" > "$test_tmp/rerun-failure-output" 2>&1; then
  fail 'recovery must exit non-zero when rerun requests keep failing'
fi
[[ "$(<"$test_tmp/rerun-failure-output")" == *'recovery had '* ]] || fail 'recovery must report rerun request failures'

rm "$test_tmp/reruns"
if env "${common_env[@]}" RECOVERY_MODE=exhausted RECOVERY_MAX_STATUS_CHECKS=2 bash "$recovery_script" > "$test_tmp/exhausted-output" 2>&1; then
  fail 'recovery must exit non-zero when checks do not finish before polling is exhausted'
fi
[[ "$(<"$test_tmp/exhausted-output")" == *'Could not confirm cancelled pull request checks'* ]] || fail 'recovery must report exhausted polling'

rm -f "$test_tmp/reruns"
env "${common_env[@]}" CANCELLED_PR_RUN_TARGETS='' RECOVERY_MODE=no-target bash "$recovery_script" > "$test_tmp/no-target-output" ||
  fail 'recovery must complete successfully when the main sweep found no PR checks to cancel'
[[ ! -s "$test_tmp/reruns" ]] || fail 'an empty cancellation manifest must not invent PR reruns'

printf 'cancelled PR check recovery tests pass\n'
