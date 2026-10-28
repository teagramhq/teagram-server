#!/usr/bin/env bash
# shellcheck disable=SC2016 # These patterns intentionally match literal workflow source.
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
[[ "$cancel_step" == *'if ! runs="$(gh api --paginate'* && "$cancel_step" == *'Could not list queued pull request CI runs; continuing without cancellation.'* ]] ||
  fail 'failure to list queued pull request runs must warn and continue'
[[ "$cancel_step" == *'if ! run_status="$(gh api'* && "$cancel_step" == *'Could not check pull request CI run'* ]] ||
  fail 'failure to recheck a pull request run must warn and continue'
[[ "$cancel_step" == *"gh run cancel \"\$run_id\" --repo \"\$GITHUB_REPOSITORY\""* && "$cancel_step" != *'gh run cancel --force'* ]] ||
  fail 'queued PR runs must use ordinary cancellation so always() cleanup can run'
[[ "$cancel_step" == *'if gh run cancel "$run_id" --repo "$GITHUB_REPOSITORY"; then'* && "$cancel_step" == *'Could not cancel queued pull request CI run'* ]] ||
  fail 'a failed cancellation must warn and leave the main priority gate successful'
[[ "$cancel_step" == *'cancelled_pr_run_ids+="${run_id} "'* ]] ||
  fail 'successfully cancelled pull request runs must be recorded for rerun'
[[ "$cancel_step" != *'status=in_progress'* && "$cancel_step" != *'/jobs?per_page=100'* ]] ||
  fail 'queued PR workflows must be cancelled even when they have started jobs'
[[ "$cancel_step" != *'grep -qx in_progress'* ]] ||
  fail 'the priority job must not query or cancel running PR workflows'
printf '%s\n' "$cancel_step" | bash -n
[[ "$(query '.jobs."prioritize-main".outputs.cancelled_pr_run_ids')" == *'steps.cancel-queued-pr-runs.outputs.cancelled_pr_run_ids'* ]] ||
  fail 'the priority job must expose cancelled run ids for requeueing'

rerun_needs="$(query '.jobs."rerun-cancelled-pr-ci".needs[]')"
[[ "$rerun_needs" == *'prioritize-main'* && "$rerun_needs" == *'lint'* ]] ||
  fail 'cancelled PR runs must be requeued after the main lint lane starts'
rerun_if="$(query '.jobs."rerun-cancelled-pr-ci".if')"
[[ "$rerun_if" == *"needs.prioritize-main.result == 'success'"* && "$rerun_if" == *"needs.prioritize-main.result == 'skipped'"* && "$rerun_if" == *"github.event_name == 'pull_request'"* && "$rerun_if" == *"github.event_name == 'push'"* && "$rerun_if" == *'needs.lint.result != '\''skipped'\'''* && "$rerun_if" == *'outputs.cancelled_pr_run_ids != '\'''\'''* ]] ||
  fail 'cancelled PR runs must be requeued only after main fan-out has started'
rerun_step="$(query '.jobs."rerun-cancelled-pr-ci".steps[] | select(.name == "Re-run cancelled pull request CI after main fan-out starts") | .run')"
[[ "$rerun_step" == *'gh run rerun "$run_id" --repo "$GITHUB_REPOSITORY"'* && "$rerun_step" == *'completed\tcancelled'* && "$rerun_step" == *'Could not re-run cancelled pull request CI run'* ]] ||
  fail 'recorded cancelled pull request runs must be rerun after main fan-out starts'
printf '%s\n' "$rerun_step" | bash -n

expected_roots=$'browser-acceptance\nbrowser-acceptance-image\nci-main\ncompose\ncss\ndeploy-workflow\ndocker\ne2e-admin\nlint\nmirror-rustfs-qualification-images\nphotothumb-runtime\nreal-server-fixtures\nrestore-readiness\nrustfs-transition-qualification\nschema-gate-postgres\nsmoke\nsql-lint\nsqlc\nsticker-bundle\nwebsocket-d1\nwebsocket-permanent-exchange'
actual_roots="$(query '.jobs | to_entries[] | select(.key != "prioritize-main" and .key != "ci" and .key != "rerun-cancelled-pr-ci" and (.value.needs == "prioritize-main" or (.value.needs[] == "prioritize-main"))) | .key' | LC_ALL=C sort)"
[[ "$actual_roots" == "$expected_roots" ]] ||
  fail 'every CI fan-out root must depend on the main priority job'

declare -A gated_cache
is_gated() {
  local job="$1"
  [[ "$job" == prioritize-main ]] && return 0
  [[ "${gated_cache[$job]:-}" == yes ]] && return 0
  [[ "${gated_cache[$job]:-}" == no ]] && return 1

  local job_needs
  job_needs="$(query ".jobs.\"$job\".needs // [] | if tag == \"!!seq\" then .[] else . end")"
  if [[ -z "$job_needs" ]]; then
    gated_cache[$job]=no
    return 1
  fi

  while IFS= read -r dependency; do
    if is_gated "$dependency"; then
      gated_cache[$job]=yes
      return 0
    fi
  done <<< "$job_needs"

  gated_cache[$job]=no
  return 1
}

all_jobs="$(query '.jobs | keys | .[]')"
while IFS= read -r job; do
  [[ -n "$job" && "$job" != prioritize-main ]] || continue
  is_gated "$job" || fail "$job can run without waiting for the main priority job"
done <<< "$all_jobs"

needs_jobs="$(query '.jobs | to_entries[] | select(.key != "prioritize-main" and .key != "publish-demo-image" and .value.needs != null) | .key')"
while IFS= read -r job; do
  [[ -n "$job" ]] || continue
  job_needs="$(query ".jobs.\"$job\".needs")"
  [[ "$job_needs" == *'prioritize-main'* ]] ||
    fail "$job must directly wait for the priority job"
  job_if="$(query ".jobs.\"$job\".if")"
  [[ "$job_if" == *"needs.prioritize-main.result == 'skipped'"* && "$job_if" == *"github.event_name == 'pull_request'"* ]] ||
    fail "$job must bypass the skipped priority job on pull requests"
done <<< "$needs_jobs"

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
ci_if="$(query '.jobs.ci.if')"
[[ "$ci_if" == *'always()'* && "$ci_if" == *"needs.prioritize-main.result == 'success'"* && "$ci_if" == *"needs.prioritize-main.result == 'failure'"* && "$ci_if" == *"needs.prioritize-main.result == 'cancelled'"* ]] ||
  fail 'the required ci summary must run to report priority-gate failures on main'
[[ "$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .env.MAIN_PRIORITY_RESULT')" == "\${{ needs.prioritize-main.result }}" ]] ||
  fail 'the required ci summary must inspect the priority result'
[[ "$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run')" == *'expected_priority_result=success'* ]] ||
  fail 'the required ci summary must fail when main priority does not succeed'
summary_run="$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run')"
[[ "$summary_run" == *'"$BROWSER_ACCEPTANCE_IMAGE_RESULT" != "success"'* && "$summary_run" != *'expected_browser_acceptance_image_result=skipped'* ]] ||
  fail 'the required ci summary must require browser images on pull requests and main pushes'

browser_image_if="$(query '.jobs."browser-acceptance-image".if')"
[[ "$browser_image_if" == *"needs.browser-acceptance.result == 'success'"* ]] ||
  fail 'browser acceptance images must run only after the runtime lane succeeds'

printf 'main CI priority contract holds\n'
