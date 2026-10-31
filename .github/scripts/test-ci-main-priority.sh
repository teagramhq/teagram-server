#!/usr/bin/env bash
# shellcheck disable=SC2016 # These patterns intentionally match literal workflow source.
set -euo pipefail

workflow="${CI_WORKFLOW:-.github/workflows/ci.yml}"
recovery_workflow="${CI_RECOVERY_WORKFLOW:-.github/workflows/recover-cancelled-pr-ci.yml}"

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

expected_gate_if="github.event_name == 'push' && github.ref == 'refs/heads/main'"
[[ "$(query '.jobs."ops-prioritize-main".name')" == 'ops: prioritize-main' ]] ||
  fail 'the priority job must use the ops stage naming contract'
[[ "$(query '.jobs."ops-prioritize-main".permissions | keys | join(",")')" == 'actions' ]] ||
  fail 'the priority job must have only Actions write permission'
gate_if="$(query '.jobs."ops-prioritize-main".if')"
[[ "$gate_if" == *"$expected_gate_if"* ]] ||
  fail 'the priority job must run only for main pushes'
[[ "$(query '.jobs."ops-prioritize-main".permissions.actions')" == 'write' ]] ||
  fail 'only the priority job needs Actions write permission to cancel queued runs'

cancel_step="$(query '.jobs."ops-prioritize-main".steps[] | select(.name == "Cancel queued pull request CI runs") | .run')"
[[ "$cancel_step" == *'event=pull_request&status=queued&per_page=100'* ]] ||
  fail 'the priority job must list queued pull request CI runs only'
[[ "$cancel_step" == *'.workflow_runs[]? | [.id, .created_at] | @tsv'* && "$cancel_step" == *'${run_id}:${run_created_at}'* ]] ||
  fail 'cancelled run IDs must retain creation timestamps for grouped recovery polling'
[[ "$cancel_step" == *'run_status" != queued'* ]] ||
  fail 'the priority job must recheck each run before cancelling it'
[[ "$cancel_step" == *'if ! runs="$(gh api --paginate'* && "$cancel_step" == *'Could not list queued pull request CI runs; continuing without cancellation.'* ]] ||
  fail 'failure to list queued pull request runs must warn and continue'
[[ "$cancel_step" == *'if ! run_status="$(gh api'* && "$cancel_step" == *'Could not check pull request CI run'* ]] ||
  fail 'failure to recheck a pull request run must warn and continue'
[[ "$cancel_step" == *'if ! started_jobs="$(gh api --paginate'* && "$cancel_step" == *'/jobs?per_page=100'* && "$cancel_step" == *'.status == "in_progress"'* && "$cancel_step" == *'.conclusion != "skipped"'* ]] ||
  fail 'the priority job must preserve PR runs once any non-skipped job has started'
[[ "$cancel_step" == *'Could not check jobs for pull request CI run'* ]] ||
  fail 'failure to inspect PR jobs must warn and preserve the run'
[[ "$cancel_step" == *"gh run cancel \"\$run_id\" --repo \"\$GITHUB_REPOSITORY\""* && "$cancel_step" != *'gh run cancel --force'* ]] ||
  fail 'queued PR runs must use ordinary cancellation so always() cleanup can run'
[[ "$cancel_step" == *'if gh run cancel "$run_id" --repo "$GITHUB_REPOSITORY"; then'* && "$cancel_step" == *'Could not cancel queued pull request CI run'* ]] ||
  fail 'a failed cancellation must warn and leave the main priority gate successful'
[[ "$cancel_step" == *'cancelled_pr_run_targets+="${run_id}:${run_created_at} "'* ]] ||
  fail 'successfully cancelled pull request runs must be recorded for rerun'
printf '%s\n' "$cancel_step" | bash -n
[[ "$(query '.jobs."ops-prioritize-main".outputs.cancelled_pr_run_targets')" == *'steps.cancel-queued-pr-runs.outputs.cancelled_pr_run_targets'* ]] ||
  fail 'the priority job must expose cancelled run ids for recovery'

dispatch_needs="$(query '.jobs."ops-dispatch-pr-ci-recovery".needs[]')"
[[ "$(query '.jobs."ops-dispatch-pr-ci-recovery".name')" == 'ops: dispatch-pr-ci-recovery' ]] ||
  fail 'the recovery dispatch job must use the ops stage naming contract'
[[ "$(query '.jobs."ops-dispatch-pr-ci-recovery".permissions | keys | join(",")')" == 'actions' ]] ||
  fail 'the recovery dispatch job must have only Actions write permission'
[[ "$dispatch_needs" == 'ops-prioritize-main' ]] ||
  fail 'the recovery dispatch must not wait for the main lint lane before evaluating its PR skip guard'
[[ "$(query '.jobs."ops-dispatch-pr-ci-recovery".timeout-minutes')" == 5 ]] ||
  fail 'dispatching PR recovery must remain short-lived in the main concurrency group'
dispatch_if="$(query '.jobs."ops-dispatch-pr-ci-recovery".if')"
dispatch_targets_if="outputs.cancelled_pr_run_targets != ''"
[[ "$dispatch_if" == *'always()'* && "$dispatch_if" == *"needs.ops-prioritize-main.result == 'success'"* && "$dispatch_if" == *"github.event_name == 'push'"* && "$dispatch_if" == *"github.ref == 'refs/heads/main'"* && "$dispatch_if" == *"$dispatch_targets_if"* && "$dispatch_if" != *'needs.lint'* ]] ||
  fail 'cancelled PR recovery must dispatch only for main pushes with cancelled runs'
dispatch_step="$(query '.jobs."ops-dispatch-pr-ci-recovery".steps[] | select(.name == "Dispatch cancelled pull request CI recovery") | .run')"
[[ "$dispatch_step" == *'gh workflow run recover-cancelled-pr-ci.yml'* && "$dispatch_step" == *'--ref main'* && "$dispatch_step" == *'-f issue=MAIN-2765'* && "$dispatch_step" == *'cancelled_pr_run_targets=$CANCELLED_PR_RUN_TARGETS'* && "$dispatch_step" != *'sleep 60'* ]] ||
  fail 'main CI must dispatch recovery with its issue and cancelled run ids without waiting'
printf '%s\n' "$dispatch_step" | bash -n

[[ "$(recovery_query '.on.workflow_dispatch.inputs.issue.required')" == true && "$(recovery_query '.on.workflow_dispatch.inputs.cancelled_pr_run_targets.required')" == true ]] ||
  fail 'the recovery workflow must require the issue key and cancelled run targets'
[[ "$(recovery_query '.concurrency // ""')" == '' ]] ||
  fail 'the delayed recovery workflow must not join the main CI concurrency group'
[[ "$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".name')" == 'ops: rerun-cancelled-pr-ci' ]] ||
  fail 'the recovery job must use the ops stage naming contract'
[[ "$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".permissions | keys | join(",")')" == 'actions,pull-requests' ]] ||
  fail 'the recovery job must have only Actions write and pull request read permissions'
[[ "$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".timeout-minutes')" == 210 ]] ||
  fail 'the recovery workflow timeout must cover its three-hour cancellation wait'
[[ "$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".permissions.actions')" == write ]] ||
  fail 'only the recovery job needs Actions write permission to rerun cancelled runs'
[[ "$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".permissions."pull-requests"')" == read ]] ||
  fail 'the recovery job needs read-only PR access to avoid rerunning stale heads'
rerun_step="$(recovery_query '.jobs."ops-rerun-cancelled-pr-ci".steps[] | select(.name == "Wait for cancellation cleanup and re-run PR CI") | .run')"
[[ "$rerun_step" == *'actions/workflows/ci.yml/runs'* && "$rerun_step" == *'--paginate'* && "$rerun_step" == *'--method GET'* && "$rerun_step" == *'created=>=$oldest_created_at'* && "$rerun_step" != *'gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}"'* ]] ||
  fail 'recovery must poll all outstanding runs together through the workflow runs list'
[[ "$rerun_step" == *'actions/runs/${run_id}/jobs?per_page=100'* && "$rerun_step" == *'select(.name != "ci" and .conclusion == "failure")'* && "$rerun_step" == *'the router already handled fail-fast'* ]] ||
  fail 'recovery must not rerun cancelled runs with a failed non-aggregate CI job'
[[ "$rerun_step" == *'.head_sha'* && "$rerun_step" == *'.pull_requests // []'* && "$rerun_step" == *'.number'* ]] ||
  fail 'recovery must read each cancelled run head SHA and associated pull request numbers'
[[ "$rerun_step" == *'pulls/${pull_request_number}'* && "$rerun_step" == *'pull_request_head_sha" != "$run_head_sha"'* && "$rerun_step" == *'pull_request_state" != open'* && "$rerun_step" == *'Skipping cancelled pull request CI run'* ]] ||
  fail 'recovery must skip closed PRs and runs whose head no longer matches the PR'
[[ "$rerun_step" == *'declare -A outstanding_pr_run_ids'* && "$rerun_step" == *'max_status_checks=181'* && "$rerun_step" == *'sleep 60'* && "$rerun_step" == *'gh run rerun "$run_id" --repo "$GITHUB_REPOSITORY"'* && "$rerun_step" == *'Could not re-run cancelled pull request CI run'* && "$rerun_step" == *'Could not confirm cancelled pull request CI runs'* ]] ||
  fail 'grouped recovery must retry, rerun completed cancellations, and report unresolved runs'
printf '%s\n' "$rerun_step" | bash -n

test_tmp="$(mktemp -d)"
trap 'rm -rf "$test_tmp"' EXIT
gh() {
  if [[ "${GH_MOCK_MODE:-}" == priority-cancel ]]; then
    if [[ "$1" == api && "$2" == --paginate ]]; then
      if [[ "$3" == "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?event=pull_request&status=queued&per_page=100" ]]; then
        [[ "$4" == --jq && "$5" == '.workflow_runs[]? | [.id, .created_at] | @tsv' ]] || return 2
        printf '123\t2026-10-28T00:00:00Z\n456\t2026-10-28T00:01:00Z\n789\t2026-10-28T00:02:00Z\n1011\t2026-10-28T00:03:00Z\n1213\t2026-10-28T00:04:00Z\n1313\t2026-10-28T00:05:00Z\n'
        return 0
      fi
      if [[ "$3" == "repos/${GITHUB_REPOSITORY}/actions/runs/"*"/jobs?per_page=100" ]]; then
        [[ "$4" == --jq && "$5" == '.jobs[]? | select(.status == "in_progress" or (.status == "completed" and .conclusion != "skipped")) | .id' ]] || return 2
        local job_run_id="${3#*runs/}"
        job_run_id="${job_run_id%%/jobs*}"
        printf '%s\n' "$job_run_id" >> "$PRIORITY_JOB_LOG"
        case "$job_run_id" in
          123|1313) ;;
          789) printf '789\n' ;;
          1011) printf '1011\n' ;;
          1213) return 2 ;;
          *) return 2 ;;
        esac
        return 0
      fi
      return 2
    fi
    if [[ "$1" == api && "$2" == "repos/${GITHUB_REPOSITORY}/actions/runs/"* ]]; then
      [[ "$3" == --jq && "$4" == '.status' ]] || return 2
      local run_id="${2##*/}"
      printf '%s\n' "$run_id" >> "$PRIORITY_STATUS_LOG"
      case "$run_id" in
        123|789|1011|1213|1313) printf 'queued\n' ;;
        456) printf 'in_progress\n' ;;
        *) return 2 ;;
      esac
      return 0
    fi
    if [[ "$1" == run && "$2" == cancel ]]; then
      [[ "$4" == --repo && "$5" == "$GITHUB_REPOSITORY" ]] || return 2
      printf '%s\n' "$3" >> "$PRIORITY_CANCEL_LOG"
      [[ "$3" != 1313 ]]
      return
    fi
    return 2
  fi

  if [[ "$1" == api ]]; then
    if [[ "$2" == --paginate && "$3" == --method ]]; then
      [[ "$3" == --method && "$4" == GET && "$5" == "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs" && " $* " == *' -f event=pull_request '* && " $* " == *'created=>='* ]] || return 2
      local reads=0
      [[ ! -f "$STATUS_READS_FILE" ]] || reads="$(<"$STATUS_READS_FILE")"
      reads=$((reads + 1))
      printf '%s\n' "$reads" > "$STATUS_READS_FILE"
      case "$STATUS_MODE" in
        after-old-bound)
          if (( reads >= 10 )); then
            printf '123\tcompleted\tcancelled\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n'
          else
            printf '123\tin_progress\t\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n'
          fi
          ;;
        stale-head)
          printf '123\tcompleted\tcancelled\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n'
          ;;
        closed-pr)
          printf '123\tcompleted\tcancelled\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n'
          ;;
        failed-job)
          printf '123\tcompleted\tcancelled\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n'
          ;;
        staggered)
          case "$reads" in
            1) printf '123\tin_progress\t\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n456\tin_progress\t\tbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\t456\n' ;;
            2) printf '123\tcompleted\tcancelled\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n456\tin_progress\t\tbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\t456\n' ;;
            *) printf '123\tcompleted\tcancelled\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n456\tcompleted\tcancelled\tbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\t456\n' ;;
          esac
          ;;
        *) printf '123\tin_progress\t\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t123\n' ;;
      esac
      return 0
    fi
    if [[ "$2" == --paginate && "$3" == "repos/${GITHUB_REPOSITORY}/actions/runs/"*"/jobs?per_page=100" ]]; then
      [[ "$4" == --jq && "$5" == '[.jobs[]? | select(.name != "ci" and .conclusion == "failure")] | length' ]] || return 2
      if [[ "$STATUS_MODE" == failed-job ]]; then
        printf '1\n'
      else
        printf '0\n'
      fi
      return 0
    fi
    if [[ "$2" == "repos/${GITHUB_REPOSITORY}/pulls/123" || "$2" == "repos/${GITHUB_REPOSITORY}/pulls/456" ]]; then
      [[ "$3" == --jq && "$4" == '[.state, .head.sha] | @tsv' ]] || return 2
      case "$STATUS_MODE:$2" in
        stale-head:*/pulls/123) printf 'open\tbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n' ;;
        closed-pr:*/pulls/123) printf 'closed\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' ;;
        *:*/pulls/123) printf 'open\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' ;;
        *:*/pulls/456) printf 'open\tbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n' ;;
      esac
      return 0
    fi
    return 2
  fi
  if [[ "$1" == run && "$2" == rerun ]]; then
    printf '%s\n' "$3" >> "$RERUN_LOG"
    return 0
  fi
  return 2
}
sleep() { :; }
export -f gh sleep

GH_MOCK_MODE=priority-cancel \
PRIORITY_STATUS_LOG="$test_tmp/priority-statuses" \
PRIORITY_JOB_LOG="$test_tmp/priority-jobs" \
PRIORITY_CANCEL_LOG="$test_tmp/priority-cancels" \
GITHUB_OUTPUT="$test_tmp/priority-output" \
GITHUB_REPOSITORY=example/repo \
  bash -c "$cancel_step" > "$test_tmp/priority-output-log" 2>&1 || fail 'the priority gate must continue when a queued-run cancellation races with a run failure'
[[ "$(<"$test_tmp/priority-statuses")" == $'123\n456\n789\n1011\n1213\n1313' ]] ||
  fail 'the priority gate must recheck every queued PR run before cancellation'
[[ "$(<"$test_tmp/priority-jobs")" == $'123\n789\n1011\n1213\n1313' ]] ||
  fail 'the priority gate must inspect jobs only for runs that still report queued'
[[ "$(<"$test_tmp/priority-cancels")" == $'123\n1313' ]] ||
  fail 'the priority gate must preserve queued-status runs with active or completed jobs'
[[ "$(<"$test_tmp/priority-output")" == 'cancelled_pr_run_targets=123:2026-10-28T00:00:00Z' ]] ||
  fail 'recovery targets must contain only successfully cancelled PR runs'
[[ "$(<"$test_tmp/priority-output-log")" == *'Could not check jobs for pull request CI run 1213'* && "$(<"$test_tmp/priority-output-log")" == *'Could not cancel queued pull request CI run 1313'* ]] ||
  fail 'a failed cancellation must warn while allowing main CI to proceed'

STATUS_MODE=after-old-bound \
STATUS_READS_FILE="$test_tmp/delayed-reads" \
RERUN_LOG="$test_tmp/delayed-reruns" \
CANCELLED_PR_RUN_TARGETS=123:2026-10-28T00:00:00Z \
ISSUE=MAIN-2765 \
GITHUB_REPOSITORY=example/repo \
  bash -c "$rerun_step" || fail 'a delayed cancelled run must eventually be rerun'
[[ "$(<"$test_tmp/delayed-reads")" == 10 && "$(<"$test_tmp/delayed-reruns")" == 123 ]] ||
  fail 'the requeue step must retry beyond the old bound and rerun once cancellation completes'

STATUS_MODE=failed-job \
STATUS_READS_FILE="$test_tmp/failed-job-reads" \
RERUN_LOG="$test_tmp/failed-job-reruns" \
CANCELLED_PR_RUN_TARGETS=123:2026-10-28T00:00:00Z \
ISSUE=MAIN-2765 \
GITHUB_REPOSITORY=example/repo \
  bash -c "$rerun_step" > "$test_tmp/failed-job-output" || fail 'a fail-fast-cancelled run must be skipped'
[[ "$(<"$test_tmp/failed-job-reads")" == 1 && ! -e "$test_tmp/failed-job-reruns" ]] ||
  fail 'recovery must not rerun a cancelled run with a failed CI job'
[[ "$(<"$test_tmp/failed-job-output")" == *'the router already handled fail-fast'* ]] ||
  fail 'recovery must explain why a router-cancelled run with failed CI was skipped'

STATUS_MODE=never \
STATUS_READS_FILE="$test_tmp/bounded-reads" \
RERUN_LOG="$test_tmp/bounded-reruns" \
CANCELLED_PR_RUN_TARGETS=123:2026-10-28T00:00:00Z \
ISSUE=MAIN-2765 \
GITHUB_REPOSITORY=example/repo \
  bash -c "$rerun_step" > "$test_tmp/bounded-output" 2>&1 || fail 'a delayed status must stop after the bounded retry count'
[[ "$(<"$test_tmp/bounded-reads")" == 181 && ! -e "$test_tmp/bounded-reruns" ]] ||
  fail 'the requeue step must stop after the bounded status checks without rerunning an active cancellation'
[[ "$(<"$test_tmp/bounded-output")" == *'after 181 status checks'* ]] ||
  fail 'the requeue step must warn when cancellation does not finish within the bound'

STATUS_MODE=stale-head \
STATUS_READS_FILE="$test_tmp/stale-head-reads" \
RERUN_LOG="$test_tmp/stale-head-reruns" \
CANCELLED_PR_RUN_TARGETS=123:2026-10-28T00:00:00Z \
ISSUE=MAIN-2765 \
GITHUB_REPOSITORY=example/repo \
  bash -c "$rerun_step" > "$test_tmp/stale-head-output" || fail 'a cancelled run for an older PR head must be skipped'
[[ "$(<"$test_tmp/stale-head-reads")" == 1 && ! -e "$test_tmp/stale-head-reruns" ]] ||
  fail 'recovery must not rerun a cancelled run after its PR head has moved'
[[ "$(<"$test_tmp/stale-head-output")" == *'not the cancelled run head'* ]] ||
  fail 'recovery must report why a stale-head run was skipped'

STATUS_MODE=closed-pr \
STATUS_READS_FILE="$test_tmp/closed-pr-reads" \
RERUN_LOG="$test_tmp/closed-pr-reruns" \
CANCELLED_PR_RUN_TARGETS=123:2026-10-28T00:00:00Z \
ISSUE=MAIN-2765 \
GITHUB_REPOSITORY=example/repo \
  bash -c "$rerun_step" > "$test_tmp/closed-pr-output" || fail 'a cancelled run for a closed PR must be skipped'
[[ "$(<"$test_tmp/closed-pr-reads")" == 1 && ! -e "$test_tmp/closed-pr-reruns" ]] ||
  fail 'recovery must not rerun a cancelled run after its PR has closed'

STATUS_MODE=staggered \
STATUS_READS_FILE="$test_tmp/staggered-reads" \
RERUN_LOG="$test_tmp/staggered-reruns" \
CANCELLED_PR_RUN_TARGETS='123:2026-10-28T00:00:00Z 456:2026-10-28T00:01:00Z' \
ISSUE=MAIN-2765 \
GITHUB_REPOSITORY=example/repo \
  bash -c "$rerun_step" || fail 'grouped recovery must rerun completed IDs while continuing to poll outstanding IDs'
[[ "$(<"$test_tmp/staggered-reads")" == 3 && "$(<"$test_tmp/staggered-reruns")" == $'123\n456' ]] ||
  fail 'recovery must poll both IDs together and rerun each as soon as cancellation completes'

expected_roots=$'build-browser-acceptance-image\nbuild-server-image\ncheck-demo-web-pin\ncheck-deploy-workflow\ncheck-security\ncheck-sqlc-drift\ncheck-sticker-license\nlint-css\nlint-go\nlint-sql\nmirror-rustfs-qualification-images\npublish-demo-image\nrestore-readiness\nrustfs-transition-qualification\ntest-browser-acceptance\ntest-compose\ntest-e2e-admin\ntest-go\ntest-migrations\ntest-photo-worker\ntest-real-server-fixtures\ntest-smoke\ntest-websocket-d1\ntest-websocket-permanent-exchange'
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

needs_jobs="$(query '.jobs | to_entries[] | select(.key != "ops-prioritize-main" and .key != "publish-demo-image" and .value.needs != null) | .key')"
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

always_jobs="$(query '.jobs | to_entries[] | select(.key != "ci" and ((.value.if // "") | contains("always()"))) | .key')"
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
[[ "$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run')" == *'expected_priority_result=success'* ]] ||
  fail 'the required ci summary must fail when main priority does not succeed'
summary_run="$(query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run')"
[[ "$summary_run" == *'"$BUILD_BROWSER_ACCEPTANCE_IMAGE_RESULT" != "success"'* && "$summary_run" != *'expected_browser_acceptance_image_result=skipped'* ]] ||
  fail 'the required ci summary must require browser images on pull requests and main pushes'

browser_image_if="$(query '.jobs."build-browser-acceptance-image".if')"
[[ "$browser_image_if" == *"needs.test-browser-acceptance.result == 'success'"* ]] ||
  fail 'browser acceptance images must run only after the runtime lane succeeds'

priority_test_step="$(query '.jobs."check-deploy-workflow".steps[] | select(.name == "Check main CI queue priority contract") | .run')"
[[ "$priority_test_step" == '.github/scripts/test-ci-main-priority.sh' ]] ||
  fail 'the deploy workflow contract job must run the priority contract test'
contract_syntax_step="$(query '.jobs."check-deploy-workflow".steps[] | select(.name == "Check workflow contract script syntax") | .run')"
[[ "$contract_syntax_step" == *'.github/scripts/test-ci-main-priority.sh'* && "$contract_syntax_step" == *'shellcheck'* ]] ||
  fail 'the deploy workflow contract job must syntax-check and lint the priority contract test'

printf 'main CI priority contract holds\n'
