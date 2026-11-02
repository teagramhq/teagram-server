#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dispatch_script="$repo_root/.github/scripts/dispatch-pr-ci-recovery.sh"
test_tmp="$(mktemp -d)"
trap 'rm -rf "$test_tmp"' EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

gh() {
  if [[ "$1" == api ]]; then
    if [[ "$2" == --paginate ]]; then
      [[ "$3" == "repos/${GITHUB_REPOSITORY}/actions/artifacts?name=${EXPECTED_ARTIFACT_NAME}&per_page=100" && "$4" == --jq ]] || return 2
      printf '%s\t700\n' "$EXPECTED_ARTIFACT_NAME"
      return 0
    fi
    if [[ "$2" == "repos/${GITHUB_REPOSITORY}/actions/runs/700" ]]; then
      [[ "$3" == --jq && "$4" == '[.name, .event, .head_branch] | @tsv' ]] || return 2
      printf '%s\n' "${MOCK_RUN_METADATA:-PR CI priority$'\t'workflow_run$'\t'main}"
      return 0
    fi
    return 2
  fi
  if [[ "$1" == run && "$2" == download ]]; then
    [[ "$3" == 700 && "$4" == --name && "$5" == "$EXPECTED_ARTIFACT_NAME" && "$6" == --dir && "$8" == --repo && "$9" == "$GITHUB_REPOSITORY" ]] || return 2
    printf '%s' "$MOCK_CANCELLED_TARGETS" > "$7/cancelled-pr-run-targets.txt"
    return 0
  fi
  if [[ "$1" == workflow && "$2" == run ]]; then
    printf '%s\n' "$@" > "$DISPATCH_ARGS_FILE"
    return 0
  fi
  return 2
}
export -f gh

for mode in empty populated; do
  expected_targets=""
  if [[ "$mode" == populated ]]; then
    expected_targets='ci.yml:101:2026-10-28T00:00:00Z perf.yml:202:2026-10-28T00:01:00Z'
  fi
  export GITHUB_REPOSITORY=example/repo
  export MAIN_RUN_ID=900
  export MAIN_RUN_ATTEMPT=1
  export MAIN_HEAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  export ISSUE=MAIN-3126
  export EXPECTED_ARTIFACT_NAME=cancelled-pr-run-targets-900-1
  export MOCK_CANCELLED_TARGETS="$expected_targets"
  export DISPATCH_ARGS_FILE="$test_tmp/$mode-dispatch-args"

  bash "$dispatch_script" || fail "recovery dispatch must accept the $mode cancellation manifest"
  grep -Fxq 'recover-cancelled-pr-ci.yml' "$DISPATCH_ARGS_FILE" || fail 'recovery dispatch must use the recovery workflow'
  grep -Fxq 'issue=MAIN-3126' "$DISPATCH_ARGS_FILE" || fail 'recovery dispatch must preserve the issue key'
  grep -Fxq 'main_run_id=900' "$DISPATCH_ARGS_FILE" || fail 'recovery dispatch must preserve the main run ID'
  grep -Fxq "cancelled_pr_run_targets=$expected_targets" "$DISPATCH_ARGS_FILE" || fail "recovery dispatch must preserve the $mode cancellation target list"
done

if env \
  DISPATCH_ARGS_FILE="$test_tmp/untrusted-dispatch-args" \
  MOCK_RUN_METADATA=$'Other workflow\tworkflow_run\tmain' \
  bash "$dispatch_script" > "$test_tmp/untrusted-output" 2>&1; then
  fail 'recovery dispatch must reject artifacts from a different workflow'
fi
[[ ! -e "$test_tmp/untrusted-dispatch-args" ]] || fail 'an untrusted artifact must not trigger PR reruns'

printf 'PR recovery dispatch artifact tests pass\n'
