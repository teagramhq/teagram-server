#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate_script="$repo_root/.github/scripts/gate-pr-checks-during-main.sh"
cancel_script="$repo_root/.github/scripts/cancel-pr-checks-for-main.sh"
test_tmp="$(mktemp -d)"
trap 'rm -rf "$test_tmp"' EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

gh() {
  if [[ "$1" == api ]]; then
    local path="$2"
    [[ "$path" != --paginate ]] || path="$3"
    if [[ "$path" == "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=100" ]]; then
      case "$GATE_MODE" in
        none) return 0 ;;
        api-error) return 2 ;;
        completed|active|missing-ci)
          printf '900\n'
          return 0
          ;;
        multiple)
          printf '900\n901\n'
          return 0
          ;;
        *) return 2 ;;
      esac
    fi
    if [[ "$path" == "repos/${GITHUB_REPOSITORY}/actions/runs/900/jobs?per_page=100" ]]; then
      case "$GATE_MODE" in
        completed) printf 'completed\n' ;;
        active) printf 'in_progress\n' ;;
        missing-ci) return 0 ;;
        multiple) printf 'in_progress\n' ;;
        *) return 2 ;;
      esac
      return 0
    fi
    if [[ "$path" == "repos/${GITHUB_REPOSITORY}/actions/runs/901/jobs?per_page=100" ]]; then
      printf 'completed\n'
      return 0
    fi
    return 2
  fi
  if [[ "$1" == run && "$2" == cancel ]]; then
    printf '%s\n' "$3" >> "$CANCEL_LOG"
    [[ "$4" == --repo && "$5" == "$GITHUB_REPOSITORY" ]] || return 2
    return 0
  fi
  return 2
}
export -f gh

for mode in none completed; do
  GATE_MODE="$mode" GITHUB_RUN_ID=700 GITHUB_REPOSITORY=example/repo CANCEL_LOG="$test_tmp/gate-cancels" \
    bash "$gate_script" > "$test_tmp/gate-$mode-output" || fail "gate should allow PR checks when mode is $mode"
done
[[ ! -e "$test_tmp/gate-cancels" ]] || fail 'the main aggregate must be complete before the gate allows PR checks'

GATE_MODE=active GITHUB_RUN_ID=701 GITHUB_REPOSITORY=example/repo CANCEL_LOG="$test_tmp/gate-cancels" \
  bash "$gate_script" > "$test_tmp/gate-active-output" || fail 'an active main aggregate must defer PR checks'
[[ "$(<"$test_tmp/gate-cancels")" == 701 ]] || fail 'the gate must cancel the deferred PR workflow run'
[[ "$(<"$test_tmp/gate-active-output")" == *'Deferring pull request checks until main CI run 900 completes'* ]] || fail 'the gate must identify the main aggregate holding the PR run'

GATE_MODE=missing-ci GITHUB_RUN_ID=702 GITHUB_REPOSITORY=example/repo CANCEL_LOG="$test_tmp/gate-cancels" \
  bash "$gate_script" > "$test_tmp/gate-missing-output" || fail 'a queued main run without its ci job must defer PR checks'
[[ "$(<"$test_tmp/gate-cancels")" == $'701\n702' ]] || fail 'the gate must defer while the main ci job has not appeared'

if GATE_MODE=api-error GITHUB_RUN_ID=703 GITHUB_REPOSITORY=example/repo CANCEL_LOG="$test_tmp/gate-cancels" \
  bash "$gate_script" > "$test_tmp/gate-error-output" 2>&1; then
  fail 'the PR gate must fail closed when it cannot inspect main CI'
fi
[[ "$(<"$test_tmp/gate-cancels")" == $'701\n702' ]] || fail 'an API error must block PR fan-out without claiming a cancellation'

export GITHUB_REPOSITORY=example/repo
export GITHUB_OUTPUT="$test_tmp/cancel-output"
export CANCEL_LOG="$test_tmp/sweep-cancels"
gh() {
  if [[ "$1" == api && "$2" == --paginate ]]; then
    local path="$3"
    case "$path" in
      *'/ci.yml/runs?event=pull_request&status=queued&per_page=100') printf '101\t2026-10-28T00:00:00Z\n' ;;
      *'/perf.yml/runs?event=pull_request&status=in_progress&per_page=100') printf '202\t2026-10-28T00:01:00Z\n' ;;
      *'/real-client-browser-flow.yml/runs?event=pull_request&status=queued&per_page=100') printf '303\t2026-10-28T00:02:00Z\n' ;;
      *) return 0 ;;
    esac
    return 0
  fi
  if [[ "$1" == run && "$2" == cancel ]]; then
    printf '%s\n' "$3" >> "$CANCEL_LOG"
    [[ "$4" == --repo && "$5" == "$GITHUB_REPOSITORY" ]] || return 2
    return 0
  fi
  return 2
}
export -f gh
bash "$cancel_script" || fail 'main priority cancellation sweep should finish'
[[ "$(<"$test_tmp/sweep-cancels")" == $'101\n202\n303' ]] || fail 'the priority sweep must cancel queued and running checks in all three workflows'
[[ "$(<"$GITHUB_OUTPUT")" == 'cancelled_pr_run_targets=ci.yml:101:2026-10-28T00:00:00Z perf.yml:202:2026-10-28T00:01:00Z real-client-browser-flow.yml:303:2026-10-28T00:02:00Z' ]] || fail 'the priority sweep must retain workflow IDs and timestamps for recovery'

printf 'PR main-CI gate and cancellation tests pass\n'
