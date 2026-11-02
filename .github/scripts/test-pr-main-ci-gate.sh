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

# This mock is invoked by the gate scripts in child Bash processes.
# shellcheck disable=SC2329
gh() {
  if [[ "$1" == api ]]; then
    local path="$2"
    [[ "$path" != --paginate ]] || path="$3"
    if [[ "$path" == "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=100" ]]; then
      case "$GATE_MODE" in
        none) return 0 ;;
        api-error) return 2 ;;
        completed|active|missing-ci|fork-active)
          printf '900\n'
          return 0
          ;;
        multiple)
          printf '901\n900\n'
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
        fork-active) printf 'in_progress\n' ;;
        multiple) printf 'in_progress\n' ;;
        *) return 2 ;;
      esac
      return 0
    fi
    if [[ "$path" == "repos/${GITHUB_REPOSITORY}/actions/runs/901/jobs?per_page=100" ]]; then
      printf 'completed\n'
      return 0
    fi
    if [[ "$path" == *"/actions/workflows/"*"/runs?event=pull_request&status="*"&per_page=100" ]]; then
      if [[ "$path" == *'/ci.yml/runs?event=pull_request&status=queued&per_page=100' ]]; then
        case "$GATE_MODE" in
          active) printf '800\t2026-10-28T00:00:00Z\t99\n801\t2026-10-28T00:01:00Z\t42\n' ;;
          multiple) printf '802\t2026-10-28T00:00:00Z\t42\n803\t2026-10-28T00:01:00Z\t99\n' ;;
        esac
      fi
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
  GATE_MODE="$mode" GITHUB_REPOSITORY=example/repo PR_NUMBER=42 PR_WORKFLOW_RUN_ID=777 GITHUB_OUTPUT="$test_tmp/gate-output" CANCEL_LOG="$test_tmp/gate-cancels" \
    bash "$gate_script" > "$test_tmp/gate-$mode-output" || fail "gate should allow PR checks when mode is $mode"
done
[[ ! -e "$test_tmp/gate-cancels" ]] || fail 'the main aggregate must be complete before the gate allows PR checks'

GATE_MODE=active GITHUB_REPOSITORY=example/repo PR_NUMBER=42 PR_WORKFLOW_RUN_ID=777 GITHUB_OUTPUT="$test_tmp/gate-output" CANCEL_LOG="$test_tmp/gate-cancels" \
  bash "$gate_script" > "$test_tmp/gate-active-output" || { cat "$test_tmp/gate-active-output" >&2; fail 'an active main aggregate must defer PR checks'; }
[[ "$(<"$test_tmp/gate-cancels")" == 801 ]] || fail 'the trusted gate must cancel the matching PR workflow run'
[[ "$(<"$test_tmp/gate-active-output")" == *'Deferring pull request checks until main CI run 900 completes'* ]] || fail 'the gate must identify the main aggregate holding the PR run'

GATE_MODE=multiple GITHUB_REPOSITORY=example/repo PR_NUMBER=42 PR_WORKFLOW_RUN_ID=777 GITHUB_OUTPUT="$test_tmp/gate-output" CANCEL_LOG="$test_tmp/gate-cancels" \
  bash "$gate_script" > "$test_tmp/gate-multiple-output" || fail 'an active main run must defer PR checks even if a completed run is listed first'
[[ "$(<"$test_tmp/gate-cancels")" == $'801\n802' ]] || fail 'the gate must continue past completed main runs and cancel matching PR checks'
[[ "$(<"$test_tmp/gate-multiple-output")" == *'main CI run 900 completes'* ]] || fail 'the gate must identify the active aggregate after a completed run'

GATE_MODE=missing-ci GITHUB_REPOSITORY=example/repo PR_NUMBER=42 PR_WORKFLOW_RUN_ID=777 GITHUB_OUTPUT="$test_tmp/gate-output" CANCEL_LOG="$test_tmp/gate-cancels" \
  bash "$gate_script" > "$test_tmp/gate-missing-output" || fail 'a queued main run without its ci job must defer PR checks'
[[ "$(<"$test_tmp/gate-cancels")" == $'801\n802' ]] || fail 'a main run without its ci job must not cancel unrelated runs'

if GATE_MODE=api-error GITHUB_REPOSITORY=example/repo PR_NUMBER=42 GITHUB_OUTPUT="$test_tmp/gate-output" CANCEL_LOG="$test_tmp/gate-cancels" \
  bash "$gate_script" > "$test_tmp/gate-error-output" 2>&1; then
  fail 'the PR gate must fail closed when it cannot inspect main CI'
fi
[[ "$(<"$test_tmp/gate-cancels")" == $'801\n802' ]] || fail 'an API error must not claim a cancellation'

GATE_MODE=fork-active GITHUB_REPOSITORY=example/repo PR_NUMBER='' PR_WORKFLOW_RUN_ID=777 GITHUB_OUTPUT="$test_tmp/fork-gate-output" CANCEL_LOG="$test_tmp/fork-gate-cancels" \
  bash "$gate_script" > "$test_tmp/fork-gate-log" || fail 'the gate must cancel a fork PR run when its PR number is unavailable'
[[ "$(<"$test_tmp/fork-gate-cancels")" == 777 ]] || fail 'the gate must cancel the triggering workflow run when pull request metadata is empty'
[[ "$(<"$test_tmp/fork-gate-log")" == *'pull request number was unavailable'* ]] || fail 'fork PR cancellation must explain why it used the workflow run ID'

export GITHUB_REPOSITORY=example/repo
export GITHUB_OUTPUT="$test_tmp/cancel-output"
export CANCEL_LOG="$test_tmp/sweep-cancels"
gh() {
  if [[ "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/actions/runs/202" ]]; then
    [[ "$3" == --jq && "$4" == '.status' ]] || return 2
    [[ "${SWEEP_MODE:-success}" == cancel-error ]] || return 2
    printf 'in_progress\n'
    return 0
  fi
  if [[ "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/actions/runs/404" ]]; then
    [[ "$3" == --jq && "$4" == '.status' && "${SWEEP_MODE:-success}" == terminal-race ]] || return 2
    printf 'completed\n'
    return 0
  fi
  if [[ "$1" == api && "$2" == --paginate ]]; then
    local path="$3"
    if [[ "${SWEEP_MODE:-success}" == list-error ]]; then
      return 2
    fi
    if [[ "${SWEEP_MODE:-success}" == terminal-race ]]; then
      case "$path" in
        *'/ci.yml/runs?event=pull_request&status=queued&per_page=100') printf '404\t2026-10-28T00:00:00Z\n' ;;
        *) return 0 ;;
      esac
      return 0
    fi
    if [[ "${SWEEP_MODE:-success}" == cancel-error ]]; then
      printf '101\t2026-10-28T00:00:00Z\n202\t2026-10-28T00:01:00Z\n'
      return 0
    fi
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
    if [[ "${SWEEP_MODE:-success}" == cancel-error && "$3" != 101 ]]; then
      return 2
    fi
    if [[ "${SWEEP_MODE:-success}" == terminal-race ]]; then
      return 2
    fi
    return 0
  fi
  return 2
}
export -f gh
bash "$cancel_script" || fail 'main priority cancellation sweep should finish'
[[ "$(<"$test_tmp/sweep-cancels")" == $'101\n202\n303' ]] || fail 'the priority sweep must cancel queued and running checks in all three workflows'
[[ "$(<"$GITHUB_OUTPUT")" == 'cancelled_pr_run_targets=ci.yml:101:2026-10-28T00:00:00Z perf.yml:202:2026-10-28T00:01:00Z real-client-browser-flow.yml:303:2026-10-28T00:02:00Z' ]] || fail 'the priority sweep must retain workflow IDs and timestamps for recovery'

if SWEEP_MODE=list-error GITHUB_REPOSITORY=example/repo GITHUB_OUTPUT="$test_tmp/list-error-output" CANCEL_LOG="$test_tmp/list-error-cancels" bash "$cancel_script" > "$test_tmp/list-error-log" 2>&1; then
  fail 'the priority sweep must fail if it cannot list PR runs'
fi

if SWEEP_MODE=cancel-error GITHUB_REPOSITORY=example/repo GITHUB_OUTPUT="$test_tmp/cancel-error-output" CANCEL_LOG="$test_tmp/cancel-error-cancels" bash "$cancel_script" > "$test_tmp/cancel-error-log" 2>&1; then
  fail 'the priority sweep must fail if it cannot cancel a PR run'
fi
[[ "$(<"$test_tmp/cancel-error-output")" == 'cancelled_pr_run_targets=ci.yml:101:2026-10-28T00:00:00Z' ]] || fail 'the failed sweep must still publish cancellations completed before its error'

SWEEP_MODE=terminal-race GITHUB_REPOSITORY=example/repo GITHUB_OUTPUT="$test_tmp/terminal-race-output" CANCEL_LOG="$test_tmp/terminal-race-cancels" \
  bash "$cancel_script" > "$test_tmp/terminal-race-log" || fail 'the priority sweep must ignore a cancel conflict only when the run is terminal'
[[ "$(<"$test_tmp/terminal-race-cancels")" == 404 ]] || fail 'the terminal-race test must exercise the cancel conflict'
[[ "$(<"$test_tmp/terminal-race-output")" == 'cancelled_pr_run_targets=' ]] || fail 'a run that completed before cancellation must not be queued for recovery'
[[ "$(<"$test_tmp/terminal-race-log")" == *'completed before it could be cancelled'* ]] || fail 'the terminal cancel race must be reported as benign'

printf 'PR main-CI gate and cancellation tests pass\n'
