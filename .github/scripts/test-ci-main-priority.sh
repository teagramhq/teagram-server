#!/usr/bin/env bash
set -euo pipefail

workflow="${CI_WORKFLOW:-.github/workflows/ci.yml}"

fail() {
  printf '%s: %s\n' "$workflow" "$1" >&2
  exit 1
}

query() {
  yq -r "$1" "$workflow"
}

expected_gate_if="github.event_name == 'push' && github.ref == 'refs/heads/main'"
gate_if="$(query '.jobs."prioritize-main".if')"
[[ "$gate_if" == *"$expected_gate_if"* ]] ||
  fail 'the priority job must run only for main pushes'
[[ "$(query '.jobs."prioritize-main".permissions.actions')" == 'write' ]] ||
  fail 'only the priority job needs Actions write permission to cancel queued runs'

cancel_step="$(query '.jobs."prioritize-main".steps[] | select(.name == "Cancel queued pull request CI runs") | .run')"
[[ "$cancel_step" == *'event=pull_request&status=queued&per_page=100'* ]] ||
  fail 'the priority job must list queued pull request CI runs only'
[[ "$cancel_step" == *'run_status" != queued'* ]] ||
  fail 'the priority job must recheck each run before cancelling it'
[[ "$cancel_step" == *'gh run cancel --force'* ]] ||
  fail 'queued PR runs must be force-cancelled so main jobs can use the runners'
[[ "$cancel_step" != *'status=in_progress'* && "$cancel_step" != *'/jobs?per_page=100'* ]] ||
  fail 'queued PR workflows must be cancelled even when they have started jobs'
[[ "$cancel_step" != *'grep -qx in_progress'* ]] ||
  fail 'the priority job must not query or cancel running PR workflows'
printf '%s\n' "$cancel_step" | bash -n

expected_roots=$'browser-acceptance\nci-main\ncompose\ncss\ndeploy-workflow\ndocker\ne2e-admin\nlint\nmirror-rustfs-qualification-images\nphotothumb-runtime\nreal-server-fixtures\nrestore-readiness\nrustfs-transition-qualification\nschema-gate-postgres\nsmoke\nsql-lint\nsqlc\nsticker-bundle\nwebsocket-d1\nwebsocket-permanent-exchange'
actual_roots="$(query '.jobs | to_entries[] | select(.key != "prioritize-main" and .key != "ci" and (.value.needs == "prioritize-main" or (.value.needs[] == "prioritize-main"))) | .key' | LC_ALL=C sort)"
[[ "$actual_roots" == "$expected_roots" ]] ||
  fail 'every CI fan-out root must depend on the main priority job'

while IFS= read -r job; do
  [[ -n "$job" ]] || continue
  job_if="$(query ".jobs.\"$job\".if")"
  [[ "$job_if" == *"needs.prioritize-main.result == 'success'"* ]] ||
    fail "$job must run after a successful priority job on main"
  [[ "$job_if" == *"needs.prioritize-main.result == 'skipped'"* && "$job_if" == *"github.event_name == 'pull_request'"* ]] ||
    fail "$job must bypass the skipped priority job on pull requests"
done <<< "$actual_roots"

always_jobs="$(query '.jobs | to_entries[] | select(.key != "ci" and ((.value.if // "") | contains("always()"))) | .key')"
while IFS= read -r job; do
  [[ -n "$job" ]] || continue
  job_needs="$(query ".jobs.\"$job\".needs")"
  [[ "$job_needs" == *'prioritize-main'* ]] ||
    fail "$job must wait for the priority job even when its condition uses always()"
  job_if="$(query ".jobs.\"$job\".if")"
  [[ "$job_if" == *"needs.prioritize-main.result == 'success'"* ]] ||
    fail "$job must stop if the main priority job fails"
  [[ "$job_if" == *"needs.prioritize-main.result == 'skipped'"* && "$job_if" == *"github.event_name == 'pull_request'"* ]] ||
    fail "$job must allow pull request runs when the priority job is skipped"
done <<< "$always_jobs"

ci_needs="$(query '.jobs.ci.needs[]')"
[[ "$ci_needs" == *'prioritize-main'* ]] ||
  fail 'the required ci summary must depend on the priority job'
[[ "$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .env.MAIN_PRIORITY_RESULT')" == '${{ needs.prioritize-main.result }}' ]] ||
  fail 'the required ci summary must inspect the priority result'
[[ "$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run')" == *'expected_priority_result=success'* ]] ||
  fail 'the required ci summary must fail when main priority does not succeed'

printf 'main CI priority contract holds\n'
