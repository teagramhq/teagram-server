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
[[ "$(query '.jobs."ops-prioritize-main".permissions | keys | sort | join(",")')" == 'contents,statuses' ]] ||
  fail 'the main gate may read commit statuses but must not hold Actions write'
[[ "$(query '.jobs."ops-prioritize-main".steps[] | select(.uses == "actions/checkout@v7.0.1") | .with.ref')" == *'github.sha'* ]] ||
  fail 'the main gate must check out the trusted main commit'
wait_if="$(query '.jobs."ops-prioritize-main".steps[] | select(.name == "Wait for trusted PR cancellation before main fan-out") | .if')"
[[ "$wait_if" == *"github.event_name == 'push' && github.ref == 'refs/heads/main'"* ]] ||
  fail 'main fan-out must wait for the trusted cancellation status'
[[ "$(query '.jobs."ops-prioritize-main".steps[] | select(.name == "Wait for trusted PR cancellation before main fan-out") | .run')" == 'bash .github/scripts/wait-for-main-priority-controller.sh' ]] ||
  fail 'the main gate must use its trusted-controller status helper'
[[ "$(query '.jobs."ops-prioritize-main".steps[] | select(.name == "Wait for trusted PR cancellation before main fan-out") | .env.MAIN_RUN_ATTEMPT')" == *'github.run_attempt'* ]] ||
  fail 'the main gate status must be unique to each workflow run attempt'
placeholder_if="$(query '.jobs."ops-prioritize-main".steps[] | select(.name == "Defer privileged priority operations to trusted workflow") | .if')"
[[ "$placeholder_if" == *"github.event_name == 'pull_request'"* ]] ||
  fail 'pull request priority must remain a read-only delegation'
[[ "$(query '.jobs."ops-prioritize-main".steps[] | select(.name == "Defer privileged priority operations to trusted workflow") | .run')" == "echo 'Pull request cancellation and recovery are handled by PR CI priority.'" ]] ||
  fail 'the pull_request workflow must delegate privileged priority operations'

[[ "$(priority_query '.on | keys | sort | join(",")')" == 'pull_request_target,workflow_run' ]] ||
  fail 'privileged priority operations must run from the base-defined trusted workflow'
[[ "$(priority_query '.on.pull_request_target.branches | join(",")')" == 'main' ]] ||
  fail 'the trusted PR controller must handle only pull requests targeting main'
[[ "$(priority_query '.on.workflow_run.workflows | join(",")')" == 'CI,RPC performance,Real-client browser flow' && "$(priority_query '.on.workflow_run.types | join(",")')" == 'requested,in_progress,completed' ]] ||
  fail 'the trusted controller must observe PR workflow starts and main CI completion'
priority_concurrency_group="$(priority_query '.concurrency.group')"
[[ "$priority_concurrency_group" == *"github.event.workflow_run.pull_requests[0].number && format('pr-{0}'"* && "$priority_concurrency_group" == *"format('run-{0}', github.event.workflow_run.id)"* ]] ||
  fail 'fork workflow_run events without PR metadata must use their triggering run ID for concurrency'
[[ "$priority_concurrency_group" == *"github.event.workflow_run.head_branch == 'main' && format('main-run-{0}-{1}', github.event.workflow_run.id, github.event.action)"* ]] ||
  fail 'main in-progress and completed workflow_run events must not replace each other in a pending concurrency group'
fork_run_a_group='pr-ci-priority-run-81001'
fork_run_b_group='pr-ci-priority-run-81002'
[[ "$fork_run_a_group" != "$fork_run_b_group" ]] ||
  fail 'concurrent fork workflow_run events must not replace one another in the same concurrency group'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".permissions | to_entries | sort_by(.key) | map(.key + "=" + .value) | join(",")')" == 'actions=write,contents=read,statuses=write' ]] ||
  fail 'only the base-defined cancellation controller may get Actions and status write'
priority_checkout_ref="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.uses == "actions/checkout@v7.0.1") | .with.ref')"
[[ "$priority_checkout_ref" == *'github.event.pull_request.base.ref'* && "$priority_checkout_ref" != *'github.event.pull_request.base.sha'* && "$priority_checkout_ref" == *'github.sha'* ]] ||
  fail 'the trusted controller must use the current base branch for its helper code'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.uses == "actions/checkout@v7.0.1") | .with."persist-credentials"')" == false ]] ||
  fail 'the trusted controller must not persist its token in git config'
gate_step="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Gate this pull request while main CI is active") | .run')"
[[ "$gate_step" == 'bash .github/scripts/gate-pr-checks-during-main.sh' ]] ||
  fail 'the trusted PR controller must use the base gate helper'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Gate this pull request while main CI is active") | .env.PR_WORKFLOW_RUN_ID')" == *'github.event.workflow_run.id'* ]] ||
  fail 'the trusted PR gate must pass the triggering run ID for fork PRs without metadata'
gate_if="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Gate this pull request while main CI is active") | .if')"
[[ "$gate_if" == *"github.event_name == 'pull_request_target'"* && "$gate_if" == *"github.event_name == 'workflow_run'"* ]] ||
  fail 'the trusted PR gate must cover the pull_request_target and workflow_run race windows'
cancel_step="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Cancel active pull request checks for main") | .run')"
[[ "$cancel_step" == 'bash .github/scripts/cancel-pr-checks-for-main.sh' ]] ||
  fail 'the trusted main handler must cancel runs in all three PR workflows'
cancel_if="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Cancel active pull request checks for main") | .if')"
[[ "$cancel_if" == *"github.event.action == 'in_progress'"* && "$cancel_if" == *"github.event.workflow_run.head_branch == 'main'"* ]] ||
  fail 'the trusted main handler must run when main CI begins'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Cancel active pull request checks for main") | .env.MAIN_RUN_ID')" == *'github.event.workflow_run.id'* ]] ||
  fail 'the trusted main sweep must inspect the exact triggering CI run'
artifact_name="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Save cancelled PR run targets") | .with.name')"
[[ "$artifact_name" == *'cancelled-pr-run-targets-'* && "$artifact_name" == *'github.event.workflow_run.id'* && "$artifact_name" == *'github.event.workflow_run.run_attempt'* ]] ||
  fail 'the trusted main handler must persist cancellation targets by main run attempt'
pending_status_step="$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Mark main priority pending") | .run')"
[[ "$pending_status_step" == *'context=main-priority-${MAIN_RUN_ID}-${MAIN_RUN_ATTEMPT}'* ]] ||
  fail 'the trusted controller must mark the matching main attempt pending before the sweep'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Write cancelled PR run targets") | .env.CANCELLED_PR_RUN_TARGETS')" == *'steps.cancel-pr-checks.outputs.cancelled_pr_run_targets'* ]] ||
  fail 'the trusted main handler must persist the cancellation sweep output'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Publish main priority result") | .env.CANCEL_STEP_RESULT')" == *'steps.cancel-pr-checks.outcome'* && "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Publish main priority result") | .env.ARTIFACT_STEP_RESULT')" == *'steps.save-cancelled-pr-run-targets.outcome'* ]] ||
  fail 'the main gate status must report cancellation and target persistence results'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Publish main priority result") | .env.MAIN_RUN_ID')" == *'github.event.workflow_run.id'* ]] ||
  fail 'the main gate status must be unique to its workflow run'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Publish main priority result") | .env.MAIN_RUN_ATTEMPT')" == *'github.event.workflow_run.run_attempt'* ]] ||
  fail 'the main gate status must be unique to each workflow run attempt'
[[ "$(priority_query '.jobs."ops-cancel-pr-checks".steps[] | select(.name == "Publish main priority result") | .env.MAIN_CI_COMPLETED')" == *'steps.cancel-pr-checks.outputs.main_ci_completed'* ]] ||
  fail 'the main gate status must report when cancellation was skipped because CI already completed'
dispatch_if="$(priority_query '.jobs."ops-dispatch-pr-ci-recovery".if')"
[[ "$dispatch_if" == *"github.event_name == 'workflow_run'"* && "$dispatch_if" == *"github.event.workflow_run.name == 'CI'"* && "$dispatch_if" == *"github.event.workflow_run.event == 'push'"* && "$dispatch_if" == *"github.event.workflow_run.head_branch == 'main'"* && "$dispatch_if" == *"github.event.action == 'completed'"* ]] ||
  fail 'the trusted dispatcher must run only after main push CI completes'
dispatch_step="$(priority_query '.jobs."ops-dispatch-pr-ci-recovery".steps[] | select((.name // "") | contains("Dispatch recovery with the main sweep")) | .run')"
[[ "$dispatch_step" == 'bash .github/scripts/dispatch-pr-ci-recovery.sh' ]] ||
  fail 'the trusted workflow must dispatch recovery with the saved cancellation targets'
[[ "$(priority_query '.jobs."ops-dispatch-pr-ci-recovery".permissions | keys | sort | join(",")')" == 'actions,contents' ]] ||
  fail 'the recovery dispatcher must have Actions write and contents read permissions'
[[ "$(priority_query '.jobs."ops-dispatch-pr-ci-recovery".steps[] | select(.uses == "actions/checkout@v7.0.1") | .with.ref')" == 'main' ]] ||
  fail 'the recovery dispatcher must check out only trusted main workflow code'
printf '%s\n' "$gate_step" "$dispatch_step" | bash -n

[[ -f .github/scripts/cancel-pr-checks-for-main.sh ]] || fail 'the cross-workflow cancellation helper must exist'
grep -q 'workflow_files=(ci.yml perf.yml real-client-browser-flow.yml)' .github/scripts/cancel-pr-checks-for-main.sh ||
  fail 'main must cancel PR CI, performance, and browser runs'
grep -q 'for run_status_filter in queued in_progress pending waiting requested' .github/scripts/cancel-pr-checks-for-main.sh ||
  fail 'main must cancel queued, running, and concurrency-pending PR checks'
bash -n .github/scripts/cancel-pr-checks-for-main.sh
bash -n .github/scripts/gate-pr-checks-during-main.sh
bash -n .github/scripts/main-ci-active-run.sh
[[ -f .github/scripts/wait-for-main-priority-controller.sh ]] || fail 'main fan-out must wait for the trusted controller'
bash -n .github/scripts/wait-for-main-priority-controller.sh
[[ -f .github/scripts/dispatch-pr-ci-recovery.sh ]] || fail 'recovery must receive the main sweep targets'
bash -n .github/scripts/dispatch-pr-ci-recovery.sh
[[ -f .github/scripts/test-wait-for-main-priority-controller.sh ]] || fail 'main controller gate tests must exist'
bash -n .github/scripts/test-wait-for-main-priority-controller.sh
[[ -f .github/scripts/test-dispatch-pr-ci-recovery.sh ]] || fail 'recovery dispatch tests must exist'
bash -n .github/scripts/test-dispatch-pr-ci-recovery.sh
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

[[ "$(workflow_query '[.jobs."ops-prioritize-main".steps[] | select((.env // {}) | has("GH_TOKEN"))] | length' .github/workflows/ci.yml)" == 1 ]] ||
  fail 'the main gate must use a read-only token only to inspect its priority status'
[[ "$(workflow_query '.jobs."ops-prioritize-main".steps[] | select(.name == "Wait for trusted PR cancellation before main fan-out") | .if' .github/workflows/ci.yml)" == *"github.event_name == 'push' && github.ref == 'refs/heads/main'"* ]] ||
  fail 'only main push fan-out may inspect the trusted priority status'

for pr_workflow in .github/workflows/perf.yml .github/workflows/real-client-browser-flow.yml; do
  [[ "$(workflow_query '[.jobs | to_entries[] | select(.key == "ops-prioritize-main" or .key == "ops-gate-pr-during-main") | .value.steps[]? | select((.env // {}) | has("GH_TOKEN"))] | length' "$pr_workflow")" == 0 ]] ||
    fail "$pr_workflow must not expose GH_TOKEN in its priority job"
  if rg -q 'gh run cancel|gh workflow run recover-cancelled-pr-ci\.yml' "$pr_workflow"; then
    fail "$pr_workflow must not perform privileged PR cancellation or recovery operations"
  fi
done
[[ -f "$threat_model" ]] || fail 'the trusted priority workflow must ship its threat model'
grep -Fq '.github/workflows/pr-ci-priority.yml' "$threat_model" || fail 'the threat model must identify the trusted controller'
grep -Fq 'does not check out or execute pull request code or artifacts' "$threat_model" || fail 'the trusted controller threat model must forbid PR code execution'
grep -Fq 'waits up to five minutes for that status before fan-out' "$threat_model" || fail 'the threat model must record the bounded main fan-out barrier'

bash .github/scripts/test-pr-main-ci-gate.sh
bash .github/scripts/test-recover-cancelled-pr-checks.sh
bash .github/scripts/test-wait-for-main-priority-controller.sh
bash .github/scripts/test-dispatch-pr-ci-recovery.sh

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
[[ "$contract_syntax_step" == *'.github/scripts/test-ci-main-priority.sh'* && "$contract_syntax_step" == *'.github/scripts/main-ci-active-run.sh'* && "$contract_syntax_step" == *'.github/scripts/wait-for-main-priority-controller.sh'* && "$contract_syntax_step" == *'.github/scripts/dispatch-pr-ci-recovery.sh'* && "$contract_syntax_step" == *'shellcheck'* ]] ||
  fail 'the deploy workflow contract job must syntax-check and lint the priority contract test'

printf 'main CI priority contract holds\n'
