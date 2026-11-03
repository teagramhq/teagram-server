#!/usr/bin/env bash
set -euo pipefail

workflow="${CI_WORKFLOW:-.github/workflows/ci.yml}"
priority_workflow="${CI_PRIORITY_WORKFLOW:-.github/workflows/pr-ci-priority.yml}"
recovery_workflow="${CI_RECOVERY_WORKFLOW:-.github/workflows/recover-cancelled-pr-ci.yml}"

fail() {
  printf '%s: %s\n' "$workflow" "$1" >&2
  exit 1
}

cancel_pattern='gh[[:space:]]+run[[:space:]]+cancel|actions/runs/[^[:space:]]+/(cancel|force-cancel)|actions/cancel'
for cancel_example in \
  'gh run cancel 123' \
  'gh api repos/example/actions/runs/123/cancel' \
  'gh api repos/example/actions/cancel' \
  'gh api repos/example/actions/runs/123/force-cancel'; do
  grep -q -E "$cancel_pattern" <<<"$cancel_example" ||
    fail "the cancellation guard misses $cancel_example"
done
if grep -R -n -E --exclude='test-ci-main-priority.sh' "$cancel_pattern" .github; then
  fail 'workflows and scripts must not cancel PR runs'
fi

if grep -nE 'ops-prioritize-main|ops-dispatch-pr-ci-recovery|recover-cancelled-pr-ci' "$workflow"; then
  fail 'CI must not gate its fan-out on cancellation or recovery jobs'
fi
[[ ! -e "$priority_workflow" ]] || fail 'the PR CI priority workflow must be absent'
[[ ! -e "$recovery_workflow" ]] || fail 'the cancelled PR recovery workflow must be absent'

[[ "$(yq -r '.jobs | has("ci")' "$workflow")" == true ]] ||
  fail 'the required CI aggregate must remain present'
expected_ci_condition="\${{ always() }}"
[[ "$(yq -r '.jobs.ci.if' "$workflow")" == "$expected_ci_condition" ]] ||
  fail 'the CI aggregate must still report failed fan-out lanes'
ci_needs="$(yq -r '.jobs.ci.needs | join(",")' "$workflow")"
[[ "$ci_needs" == *check-security* ]] ||
  fail 'the CI aggregate must still depend on required fan-out lanes'
[[ "$ci_needs" == *test-go* ]] ||
  fail 'the CI aggregate must still depend on the Go test lane'

printf 'PR CI cancellation policy holds\n'
