#!/usr/bin/env bash
# shellcheck disable=SC2016 # These patterns intentionally match literal workflow source.
set -euo pipefail

workflow="${CI_WORKFLOW:-.github/workflows/ci.yml}"
recovery_workflow="${CI_RECOVERY_WORKFLOW:-.github/workflows/recover-cancelled-pr-ci.yml}"
priority_workflow="${CI_PRIORITY_WORKFLOW:-.github/workflows/pr-ci-priority.yml}"
threat_model="${CI_PRIORITY_THREAT_MODEL:-docs/ci-priority-threat-model.md}"

fail() {
  printf '%s: %s\n' "$workflow" "$1" >&2
  exit 1
}

query() {
  yq -r "$1" "$workflow"
}

recovery_query() {
  yq -r "$1" "$recovery_workflow"
}

priority_query() {
  yq -r "$1" "$priority_workflow"
}

workflow_query() {
  yq -r "$1" "$2"
}

priority_if="$(query '.jobs."ops-prioritize-main".if')"
[[ "$(query '.jobs."ops-prioritize-main".name')" == 'ops: prioritize-main' ]] ||
  fail 'the priority job must use the ops stage naming contract'
[[ "$priority_if" == *"github.event_name == 'push' && github.ref == 'refs/heads/main'"* && "$priority_if" == *"github.event_name == 'pull_request'"* ]] ||
  fail 'the priority placeholder must remain in the CI dependency graph for main and pull request runs'
[[ "$(query '.jobs."ops-prioritize-main".permissions | keys | join(",")')" == 'contents' ]] ||
  fail 'the pull_request workflow must not hold an Actions write token for priority operations'
[[ "$(query '[.jobs."ops-prioritize-main".steps[] | select((.env // {}) | has("GH_TOKEN"))] | length')" == 0 ]] ||
  fail 'the pull_request workflow must not expose GH_TOKEN in its priority job'
[[ "$(query '.jobs."ops-prioritize-main".steps[] | select(.name == "Defer privileged priority operations to trusted workflow") | .run')" == "echo 'Pull request cancellation and recovery are handled by PR CI priority.'" ]] ||
  fail 'the pull_request workflow must delegate privileged priority operations'

[[ "$(priority_query '.on | keys | sort | join(",")')" == 'pull_request_target,push,workflow_run' ]] ||
  fail 'privileged priority operations must run from the base-defined trusted workflow'
[[ "$(priority_query '.on.pull_request_target.branches | join(",")')" == 'main' ]] ||
  fail 'the trusted PR controller must handle only pull requests targeting main'
[[ "$(priority_query '.on.workflow_run.workflows | join(",")')" == 'CI,RPC performance,Real-client browser flow' && "$(priority_query '.on.workflow_run.types | join(",")')" == 'requested,in_progress,completed' ]] ||
  fail 'the trusted controller must observe PR workflow starts and main CI completion'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".permissions | to_entries | sort_by(.key) | map(.key + "=" + .value) | join(",")')" == 'actions=write,contents=read' ]] ||
  fail 'only the base-defined cancellation controller may get Actions write'
priority_checkout_ref="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.uses == "actions/checkout@v7.0.1") | .with.ref')"
[[ "$priority_checkout_ref" == *'github.event.pull_request.base.sha'* && "$priority_checkout_ref" == *'github.sha'* ]] ||
  fail 'the trusted controller must use the base commit for its helper code'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.uses == "actions/checkout@v7.0.1") | .with."persist-credentials"')" == false ]] ||
  fail 'the trusted controller must not persist its token in git config'
gate_step="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Gate this pull request while main CI is active") | .run')"
[[ "$gate_step" == 'bash .github/scripts/gate-pr-checks-during-main.sh' ]] ||
  fail 'the trusted PR controller must use the base gate helper'
gate_if="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Gate this pull request while main CI is active") | .if')"
[[ "$gate_if" == *"github.event_name == 'pull_request_target'"* && "$gate_if" == *"github.event_name == 'workflow_run'"* ]] ||
  fail 'the trusted PR gate must cover the pull_request_target and workflow_run race windows'
cancel_step="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Cancel active pull request checks for main") | .run')"
[[ "$cancel_step" == 'bash .github/scripts/cancel-pr-checks-for-main.sh' ]] ||
  fail 'the trusted main-push handler must cancel runs in all three PR workflows'
dispatch_if="$(priority_query '.jobs."ops-dispatch-pr-ci-recovery".if')"
[[ "$dispatch_if" == *"github.event_name == 'workflow_run'"* && "$dispatch_if" == *"github.event.workflow_run.name == 'CI'"* && "$dispatch_if" == *"github.event.workflow_run.event == 'push'"* && "$dispatch_if" == *"github.event.workflow_run.head_branch == 'main'"* ]] ||
  fail 'the trusted dispatcher must run only after main push CI completes'
dispatch_step="$(priority_query '.jobs."ops-dispatch-pr-ci-recovery".steps[] | select(.name == "Dispatch cancelled pull request CI recovery") | .run')"
[[ "$dispatch_step" == *'gh workflow run recover-cancelled-pr-ci.yml'* && "$dispatch_step" == *'--ref main'* && "$dispatch_step" == *'-f issue=MAIN-3126'* && "$dispatch_step" == *'main_run_id=$MAIN_RUN_ID'* && "$dispatch_step" == *'exit 1'* ]] ||
  fail 'the trusted workflow must dispatch recovery after the main aggregate'
[[ "$(priority_query '.jobs."ops-dispatch-pr-ci-recovery".permissions | keys | join(",")')" == 'actions' ]] ||
  fail 'the recovery dispatcher must have only Actions write permission'
printf '%s\n' "$gate_step" "$dispatch_step" | bash -n

[[ -f .github/scripts/cancel-pr-checks-for-main.sh ]] || fail 'the cross-workflow cancellation helper must exist'
grep -q 'workflow_files=(ci.yml perf.yml real-client-browser-flow.yml)' .github/scripts/cancel-pr-checks-for-main.sh ||
  fail 'main must cancel PR CI, performance, and browser runs'
grep -q 'for run_status_filter in queued in_progress pending waiting requested' .github/scripts/cancel-pr-checks-for-main.sh ||
  fail 'main must cancel queued, running, and concurrency-pending PR checks'
bash -n .github/scripts/cancel-pr-checks-for-main.sh
bash -n .github/scripts/gate-pr-checks-during-main.sh
bash -n .github/scripts/main-ci-active-run.sh
grep -q 'main-ci-active-run.sh' .github/scripts/gate-pr-checks-during-main.sh ||
  fail 'the PR gate must use the shared main-CI activity predicate'
grep -Fq '# shellcheck source=.github/scripts/main-ci-active-run.sh' .github/scripts/gate-pr-checks-during-main.sh ||
  fail 'ShellCheck must resolve the PR gate shared helper from the repository root'
grep -Fq '# shellcheck source=.github/scripts/main-ci-active-run.sh' .github/scripts/recover-cancelled-pr-checks.sh ||
  fail 'ShellCheck must resolve the recovery shared helper from the repository root'

[[ "$(recovery_query '.on.workflow_dispatch.inputs.issue.required')" == true && "$(recovery_query '.on.workflow_dispatch.inputs.cancelled_pr_run_targets.required')" == false && "$(recovery_query '.on.workflow_dispatch.inputs.main_run_id.required')" == true ]] ||
  fail 'recovery must require its issue, main run ID, and cancellation targets'
[[ "$(recovery_query '.on.workflow_dispatch.inputs.main_concurrency_group')" == 'null' && "$(recovery_query '.concurrency')" == 'null' ]] ||
  fail 'recovery must not take or share the main concurrency group'
[[ "$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".permissions | keys | sort | join(",")')" == 'actions,contents,pull-requests' ]] ||
  fail 'recovery permissions must be limited to Actions write, contents read, and pull request read'
[[ "$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".timeout-minutes')" == 210 ]] ||
  fail 'recovery must retain enough time for cancellation cleanup'
rerun_step="$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".steps[] | select(.name == "Wait for main CI and re-run current PR checks") | .run')"
[[ "$rerun_step" == 'bash .github/scripts/recover-cancelled-pr-checks.sh' ]] ||
  fail 'recovery must use the current-head checks helper'
bash -n .github/scripts/recover-cancelled-pr-checks.sh
grep -q 'main_ci_active_run_id' .github/scripts/recover-cancelled-pr-checks.sh ||
  fail 'recovery must wait for main CI to become idle before every rerun'

for pr_workflow in .github/workflows/perf.yml .github/workflows/real-client-browser-flow.yml; do
  [[ "$(workflow_query '.jobs."ops-gate-pr-during-main".permissions | keys | join(",")' "$pr_workflow")" == 'contents' ]] ||
    fail "$pr_workflow must not hold an Actions write token for main prioritization"
  [[ "$(workflow_query '[.jobs."ops-gate-pr-during-main".steps[] | select((.env // {}) | has("GH_TOKEN"))] | length' "$pr_workflow")" == 0 ]] ||
    fail "$pr_workflow must not expose GH_TOKEN in its priority job"
  [[ "$(workflow_query '.jobs."ops-gate-pr-during-main".steps[] | select(.name == "Defer privileged priority operations to trusted workflow") | .run' "$pr_workflow")" == "echo 'Pull request cancellation and recovery are handled by PR CI priority.'" ]] ||
    fail "$pr_workflow must delegate privileged priority operations"
done

for pr_workflow in .github/workflows/ci.yml .github/workflows/perf.yml .github/workflows/real-client-browser-flow.yml; do
  [[ "$(workflow_query '[.jobs | to_entries[] | select(.key == "ops-prioritize-main" or .key == "ops-gate-pr-during-main") | .value.steps[]? | select((.env // {}) | has("GH_TOKEN"))] | length' "$pr_workflow")" == 0 ]] ||
    fail "$pr_workflow must not expose GH_TOKEN in its priority job"
  if rg -q 'gh run cancel|gh workflow run recover-cancelled-pr-ci\.yml' "$pr_workflow"; then
    fail "$pr_workflow must not perform privileged PR cancellation or recovery operations"
  fi
done
[[ -f "$threat_model" ]] || fail 'the trusted priority workflow must ship its threat model'
grep -Fq '.github/workflows/pr-ci-priority.yml' "$threat_model" || fail 'the threat model must identify the trusted controller'
grep -Fq 'does not check out or execute pull request code or artifacts' "$threat_model" || fail 'the trusted controller threat model must forbid PR code execution'

bash .github/scripts/test-pr-main-ci-gate.sh
bash .github/scripts/test-recover-cancelled-pr-checks.sh

expected_roots=$'build-browser-acceptance-image\nbuild-server-image\ncheck-demo-web-pin\ncheck-deploy-workflow\ncheck-security\ncheck-sqlc-drift\ncheck-sticker-license\nlint-css\nlint-go\nlint-sql\npublish-demo-image\ntest-browser-acceptance\ntest-compose\ntest-e2e-admin\ntest-git-hooks\ntest-go\ntest-migrations\ntest-photo-worker\ntest-real-server-fixtures\ntest-smoke\ntest-websocket-d1\ntest-websocket-permanent-exchange'
actual_roots="$(query '.jobs | to_entries[] | select(.key != "ops-prioritize-main" and .key != "ci" and .key != "ops-dispatch-pr-ci-recovery" and (.value.needs == "ops-prioritize-main" or (.value.needs[] == "ops-prioritize-main"))) | .key' | LC_ALL=C sort)"
[[ "$actual_roots" == "$expected_roots" ]] ||
  fail 'every CI fan-out root must depend on the main priority job'

expected_pr_gated_jobs="$(printf '%s\n' "$expected_roots" | grep -v '^publish-demo-image$')"
actual_pr_gated_jobs="$(query '.jobs | to_entries[] | select((.value.if // "") | contains("!cancelled()")) | .key' | LC_ALL=C sort)"
[[ "$actual_pr_gated_jobs" == "$expected_pr_gated_jobs" ]] ||
  fail 'every pull request CI job except the final aggregator must stop after cancellation'
[[ "$(query '.jobs.ci.if | contains("always()")')" == true ]] ||
  fail 'the final ci aggregator must run after cancellation to report a red result'
cancel_step_count="$(query '[.jobs[] | .steps[]? | select(.name == "Cancel PR workflow run after job failure")] | length')"
[[ "$cancel_step_count" == 0 ]] ||
  fail 'PR jobs must leave run cancellation to the CI router'
while IFS= read -r job; do
  [[ -n "$job" ]] || continue
  job_if="$(query ".jobs.\"$job\".if")"
  [[ "$job_if" == *"github.event_name == 'pull_request' && !cancelled()"* && "$job_if" == *"github.event_name != 'pull_request' && always()"* ]] ||
    fail "$job must stop after a PR cancellation while retaining main-push gates"
done <<< "$actual_pr_gated_jobs"

declare -A gated_cache
is_gated() {
  local job="$1"
  [[ "$job" == ops-prioritize-main ]] && return 0
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
  [[ -n "$job" && "$job" != ops-prioritize-main ]] || continue
  is_gated "$job" || fail "$job can run without waiting for the main priority job"
done <<< "$all_jobs"

needs_jobs="$(query '.jobs | to_entries[] | select(.key != "ops-prioritize-main" and .key != "ops-dispatch-pr-ci-recovery" and .key != "publish-demo-image" and .value.needs != null) | .key')"
while IFS= read -r job; do
  [[ -n "$job" ]] || continue
  job_needs="$(query ".jobs.\"$job\".needs")"
  [[ "$job_needs" == *'ops-prioritize-main'* ]] ||
    fail "$job must directly wait for the priority job"
  job_if="$(query ".jobs.\"$job\".if")"
  [[ "$job_if" == *"needs.ops-prioritize-main.result == 'skipped'"* && "$job_if" == *"github.event_name == 'pull_request'"* ]] ||
    fail "$job must bypass the skipped priority job on pull requests"
done <<< "$needs_jobs"

while IFS= read -r job; do
  [[ -n "$job" ]] || continue
  job_if="$(query ".jobs.\"$job\".if")"
  [[ "$job_if" == *"needs.ops-prioritize-main.result == 'success'"* ]] ||
    fail "$job must run after a successful priority job on main"
  [[ "$job_if" == *"needs.ops-prioritize-main.result == 'skipped'"* && "$job_if" == *"github.event_name == 'pull_request'"* ]] ||
    fail "$job must bypass the skipped priority job on pull requests"
done <<< "$actual_roots"

always_jobs="$(query '.jobs | to_entries[] | select(.key != "ci" and .key != "ops-dispatch-pr-ci-recovery" and ((.value.if // "") | contains("always()"))) | .key')"
while IFS= read -r job; do
  [[ -n "$job" ]] || continue
  job_needs="$(query ".jobs.\"$job\".needs")"
  [[ "$job_needs" == *'ops-prioritize-main'* ]] ||
    fail "$job must wait for the priority job even when its condition uses always()"
  job_if="$(query ".jobs.\"$job\".if")"
  [[ "$job_if" == *"needs.ops-prioritize-main.result == 'success'"* ]] ||
    fail "$job must stop if the main priority job fails"
  [[ "$job_if" == *"needs.ops-prioritize-main.result == 'skipped'"* && "$job_if" == *"github.event_name == 'pull_request'"* ]] ||
    fail "$job must allow pull request runs when the priority job is skipped"
done <<< "$always_jobs"

ci_needs="$(query '.jobs.ci.needs[]')"
[[ "$ci_needs" == *'ops-prioritize-main'* ]] ||
  fail 'the required ci summary must depend on the priority job'
ci_if="$(query '.jobs.ci.if')"
[[ "$ci_if" == *'always()'* && "$ci_if" == *"needs.ops-prioritize-main.result == 'success'"* && "$ci_if" == *"needs.ops-prioritize-main.result == 'failure'"* && "$ci_if" == *"needs.ops-prioritize-main.result == 'cancelled'"* ]] ||
  fail 'the required ci summary must run to report priority-gate failures on main'
[[ "$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .env.MAIN_PRIORITY_RESULT')" == "\${{ needs.ops-prioritize-main.result }}" ]] ||
  fail 'the required ci summary must inspect the priority result'
summary_run="$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run')"
summary_env=(GITHUB_EVENT_NAME=pull_request)
while IFS= read -r env_name; do
  summary_env+=("$env_name=success")
done < <(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .env | keys | .[]')
env "${summary_env[@]}" bash -e -c "$summary_run" > /dev/null ||
  fail 'the required ci summary must accept a successful priority job on pull requests'
[[ "$summary_run" == *'"$MAIN_PRIORITY_RESULT" != "success"'* ]] ||
  fail 'the required ci summary must require a successful priority job'
[[ "$summary_run" == *'"$BUILD_BROWSER_ACCEPTANCE_IMAGE_RESULT" != "success"'* && "$summary_run" != *'expected_browser_acceptance_image_result=skipped'* ]] ||
  fail 'the required ci summary must require browser images on pull requests and main pushes'

browser_image_if="$(query '.jobs."build-browser-acceptance-image".if')"
[[ "$browser_image_if" == *"needs.test-browser-acceptance.result == 'success'"* ]] ||
  fail 'browser acceptance images must run only after the runtime lane succeeds'

priority_test_step="$(query '.jobs."check-deploy-workflow".steps[] | select(.name == "Check main CI queue priority contract") | .run')"
[[ "$priority_test_step" == '.github/scripts/test-ci-main-priority.sh' ]] ||
  fail 'the deploy workflow contract job must run the priority contract test'
contract_syntax_step="$(query '.jobs."check-deploy-workflow".steps[] | select(.name == "Check workflow contract script syntax") | .run')"
[[ "$contract_syntax_step" == *'.github/scripts/test-ci-main-priority.sh'* && "$contract_syntax_step" == *'.github/scripts/main-ci-active-run.sh'* && "$contract_syntax_step" == *'shellcheck'* ]] ||
  fail 'the deploy workflow contract job must syntax-check and lint the priority contract test'

printf 'main CI priority contract holds\n'
