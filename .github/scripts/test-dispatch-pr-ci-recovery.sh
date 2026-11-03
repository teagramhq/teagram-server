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
  local artifact_lookup_count=0
  if [[ "$1" == api ]]; then
    if [[ "$2" == --paginate ]]; then
      [[ "$4" == --jq ]] || return 2
      if [[ "$3" == "repos/${GITHUB_REPOSITORY}/actions/runs/${MAIN_RUN_ID}/jobs?per_page=100" ]]; then
        [[ "${MOCK_CI_JOB_PRESENT:-yes}" == no ]] || printf '701\n'
        return 0
      fi
      if [[ "$3" == "repos/${GITHUB_REPOSITORY}/actions/artifacts?name=${EXPECTED_ARTIFACT_NAME}&per_page=100" ]]; then
        if [[ -f "${ARTIFACT_LOOKUPS_FILE:-}" ]]; then
          artifact_lookup_count="$(<"$ARTIFACT_LOOKUPS_FILE")"
        fi
        artifact_lookup_count=$((artifact_lookup_count + 1))
        [[ -z "${ARTIFACT_LOOKUPS_FILE:-}" ]] || printf '%s\n' "$artifact_lookup_count" > "$ARTIFACT_LOOKUPS_FILE"
        [[ "${MOCK_ARTIFACT_MODE:-present}" != no-manifest ]] || return 0
        if [[ "${MOCK_ARTIFACT_MODE:-present}" == late-manifest ]] && (( artifact_lookup_count == 1 )); then
          return 0
        fi
        printf '%s\t700\n' "$EXPECTED_ARTIFACT_NAME"
        return 0
      fi
      return 2
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
sleep() { :; }
export -f gh sleep

for mode in empty populated late-manifest; do
  expected_targets=""
  if [[ "$mode" != empty ]]; then
    expected_targets='ci.yml:101:2026-10-28T00:00:00Z perf.yml:202:2026-10-28T00:01:00Z'
  fi
  export GITHUB_REPOSITORY=example/repo
  export MAIN_RUN_ID=900
  export MAIN_RUN_ATTEMPT=1
  export MAIN_HEAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  export ISSUE=MAIN-3126
  export EXPECTED_ARTIFACT_NAME=cancelled-pr-run-targets-900-1
  export MOCK_CANCELLED_TARGETS="$expected_targets"
  export MOCK_ARTIFACT_MODE=present
  [[ "$mode" != late-manifest ]] || export MOCK_ARTIFACT_MODE=late-manifest
  export RECOVERY_MANIFEST_MAX_CHECKS=2
  export RECOVERY_MANIFEST_CHECK_INTERVAL_SECONDS=0
  export ARTIFACT_LOOKUPS_FILE="$test_tmp/$mode-artifact-lookups"
  export DISPATCH_ARGS_FILE="$test_tmp/$mode-dispatch-args"

  bash "$dispatch_script" || fail "recovery dispatch must accept the $mode cancellation manifest"
  grep -Fxq 'recover-cancelled-pr-ci.yml' "$DISPATCH_ARGS_FILE" || fail 'recovery dispatch must use the recovery workflow'
  grep -Fxq 'issue=MAIN-3126' "$DISPATCH_ARGS_FILE" || fail 'recovery dispatch must preserve the issue key'
  grep -Fxq 'main_run_id=900' "$DISPATCH_ARGS_FILE" || fail 'recovery dispatch must preserve the main run ID'
  grep -Fxq "cancelled_pr_run_targets=$expected_targets" "$DISPATCH_ARGS_FILE" || fail "recovery dispatch must preserve the $mode cancellation target list"
  if [[ "$mode" == late-manifest ]]; then
    [[ "$(<"$ARTIFACT_LOOKUPS_FILE")" == 2 ]] || fail 'recovery dispatch must retry the manifest lookup until the priority handler uploads it'
  fi
done

env \
  MOCK_CI_JOB_PRESENT=no \
  MOCK_ARTIFACT_MODE=no-manifest \
  DISPATCH_ARGS_FILE="$test_tmp/never-started-dispatch-args" \
  bash "$dispatch_script" > "$test_tmp/never-started-output" 2>&1 || fail 'a main run with no ci job must skip recovery successfully'
grep -Fq 'never started and has no ci job' "$test_tmp/never-started-output" || fail 'a superseded main run must explain why recovery was skipped'
[[ ! -e "$test_tmp/never-started-dispatch-args" ]] || fail 'a main run without a ci job must not dispatch recovery'

if env \
  MOCK_CI_JOB_PRESENT=yes \
  MOCK_ARTIFACT_MODE=no-manifest \
  RECOVERY_MANIFEST_MAX_CHECKS=2 \
  RECOVERY_MANIFEST_CHECK_INTERVAL_SECONDS=0 \
  ARTIFACT_LOOKUPS_FILE="$test_tmp/missing-manifest-lookups" \
  DISPATCH_ARGS_FILE="$test_tmp/started-without-manifest-dispatch-args" \
  bash "$dispatch_script" > "$test_tmp/started-without-manifest-output" 2>&1; then
  fail 'a started main run without its cancellation manifest must fail'
fi
grep -Fq 'Expected one cancellation manifest' "$test_tmp/started-without-manifest-output" || fail 'a started main run must retain the missing-manifest error'
[[ ! -e "$test_tmp/started-without-manifest-dispatch-args" ]] || fail 'a started main run without a manifest must not dispatch recovery'
[[ "$(<"$test_tmp/missing-manifest-lookups")" == 2 ]] || fail 'recovery dispatch must retry the manifest lookup before failing'

if env \
  DISPATCH_ARGS_FILE="$test_tmp/untrusted-dispatch-args" \
  MOCK_RUN_METADATA=$'Other workflow\tworkflow_run\tmain' \
  bash "$dispatch_script" > "$test_tmp/untrusted-output" 2>&1; then
  fail 'recovery dispatch must reject artifacts from a different workflow'
fi
[[ ! -e "$test_tmp/untrusted-dispatch-args" ]] || fail 'an untrusted artifact must not trigger PR reruns'

printf 'PR recovery dispatch artifact tests pass\n'
