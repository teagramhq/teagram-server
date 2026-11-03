#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${MAIN_RUN_ID:?MAIN_RUN_ID is required}"
: "${ISSUE:?ISSUE is required}"

workflow_files=(ci.yml perf.yml real-client-browser-flow.yml)
declare -A outstanding_pr_run_ids=()
declare -A oldest_created_at_by_workflow=()
declare -A handled_pr_run_ids=()
declare -A rerun_requested_attempt_by_run=()
recovery_failure_count=0

# shellcheck source=.github/scripts/main-ci-active-run.sh
source "$(dirname "${BASH_SOURCE[0]}")/main-ci-active-run.sh"

if [[ ! "$MAIN_RUN_ID" =~ ^[0-9]+$ ]]; then
  echo "::error::Invalid main workflow run ID; cannot recover deferred pull request checks."
  exit 1
fi

if ! main_started_at="$(gh api \
  "repos/${GITHUB_REPOSITORY}/actions/runs/${MAIN_RUN_ID}" \
  --jq '.run_started_at // ""' 2>/dev/null)"; then
  echo "::error::Could not read the main workflow start time; cannot recover deferred pull request checks."
  exit 1
fi
if [[ ! "$main_started_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$ ]]; then
  echo "::error::Invalid main workflow start time; cannot recover deferred pull request checks."
  exit 1
fi
main_started_at="${main_started_at%Z}"
main_started_at="${main_started_at%%.*}Z"

latest_workflow_run_attempt_for_head() {
  local workflow_file="$1"
  local head_sha="$2"
  local matching_runs candidate_id candidate_attempt candidate_created_at latest_id="" latest_attempt="" latest_created_at=""

  if ! matching_runs="$(gh api --paginate \
    "repos/${GITHUB_REPOSITORY}/actions/workflows/${workflow_file}/runs?event=pull_request&head_sha=${head_sha}&per_page=100" \
    --jq '.workflow_runs[]? | [.id, (.run_attempt // 1), .created_at] | @tsv' 2>/dev/null)"; then
    return 2
  fi

  while IFS=$'\t' read -r candidate_id candidate_attempt candidate_created_at; do
    [[ "$candidate_id" =~ ^[0-9]+$ && "$candidate_attempt" =~ ^[0-9]+$ ]] || continue
    [[ "$candidate_created_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$ ]] || continue
    candidate_created_at="${candidate_created_at%Z}"
    candidate_created_at="${candidate_created_at%%.*}Z"
    if [[ -z "$latest_id" || "$candidate_created_at" > "$latest_created_at" || ( "$candidate_created_at" == "$latest_created_at" && "$candidate_id" -gt "$latest_id" ) ]]; then
      latest_id="$candidate_id"
      latest_attempt="$candidate_attempt"
      latest_created_at="$candidate_created_at"
    fi
  done <<< "$matching_runs"

  [[ -n "$latest_id" ]] || return 1
  printf '%s\t%s\n' "$latest_id" "$latest_attempt"
}

has_open_pull_request_with_head_sha() {
  local head_sha="$1"
  local matching_pull_request_numbers pull_request_number

  if ! matching_pull_request_numbers="$(gh api --paginate \
    "repos/${GITHUB_REPOSITORY}/commits/${head_sha}/pulls?per_page=100" \
    --jq ".[] | select(.state == \"open\" and .head.sha == \"${head_sha}\") | .number" 2>/dev/null)"; then
    return 2
  fi
  [[ -n "$matching_pull_request_numbers" ]] || return 1

  while IFS= read -r pull_request_number; do
    [[ "$pull_request_number" =~ ^[0-9]+$ ]] || return 2
  done <<< "$matching_pull_request_numbers"
  return 0
}

for workflow_file in "${workflow_files[@]}"; do
  oldest_created_at_by_workflow["$workflow_file"]="$main_started_at"
done

read -r -a cancelled_pr_run_targets <<< "${CANCELLED_PR_RUN_TARGETS:-}"
for run_target in "${cancelled_pr_run_targets[@]}"; do
  if [[ ! "$run_target" =~ ^(ci\.yml|perf\.yml|real-client-browser-flow\.yml):([0-9]+):([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z)$ ]]; then
    echo "::error::Ignoring invalid cancelled pull request run target."
    recovery_failure_count=$((recovery_failure_count + 1))
    continue
  fi
  workflow_file="${BASH_REMATCH[1]}"
  run_id="${BASH_REMATCH[2]}"
  run_created_at="${BASH_REMATCH[3]%Z}"
  run_created_at="${run_created_at%%.*}Z"
  run_key="${workflow_file}|${run_id}"
  outstanding_pr_run_ids["$run_key"]="$run_created_at"
  if [[ "$run_created_at" < "${oldest_created_at_by_workflow[$workflow_file]}" ]]; then
    oldest_created_at_by_workflow["$workflow_file"]="$run_created_at"
  fi
done

max_status_checks="${RECOVERY_MAX_STATUS_CHECKS:-181}"
if [[ ! "$max_status_checks" =~ ^[0-9]+$ ]] || (( max_status_checks < 1 )); then
  echo "::error::Invalid recovery status check limit."
  exit 1
fi
for ((status_check_attempt = 1; status_check_attempt <= max_status_checks; status_check_attempt++)); do
  for workflow_file in "${workflow_files[@]}"; do
    oldest_created_at="${oldest_created_at_by_workflow[$workflow_file]}"
    # Preserve missing PR associations as a sentinel because Bash read collapses empty tab-delimited fields.
    if ! runs="$(gh api --paginate \
      --method GET \
      "repos/${GITHUB_REPOSITORY}/actions/workflows/${workflow_file}/runs" \
      -f event=pull_request \
      -f "created=>=$oldest_created_at" \
      -F per_page=100 \
      --jq '.workflow_runs[]? | [.id, .status, (.conclusion // "none"), .head_sha, ([.pull_requests // [] | .[] | .number] | join(",") | if . == "" then "-" else . end), .created_at, (.run_attempt // 1)] | @tsv' 2>/dev/null)"; then
      echo "::error::Could not list pull request checks for ${workflow_file}; retrying."
      recovery_failure_count=$((recovery_failure_count + 1))
      runs=""
    fi

    while IFS=$'\t' read -r run_id run_status run_conclusion run_head_sha run_pr_numbers run_created_at run_attempt; do
      [[ "$run_id" =~ ^[0-9]+$ ]] || continue
      [[ "$run_attempt" =~ ^[0-9]+$ ]] || run_attempt=1
      run_created_at="${run_created_at%Z}"
      run_created_at="${run_created_at%%.*}Z"
      run_key="${workflow_file}|${run_id}"
      [[ -z "${handled_pr_run_ids[$run_key]+present}" ]] || continue

      if [[ -z "${outstanding_pr_run_ids[$run_key]+present}" && "$run_created_at" < "$main_started_at" ]]; then
        continue
      fi
      if [[ -z "${outstanding_pr_run_ids[$run_key]+present}" ]]; then
        if [[ "$run_status" != completed || "$run_conclusion" != cancelled ]]; then
          continue
        fi
        outstanding_pr_run_ids["$run_key"]="$run_created_at"
      fi
      if [[ "$run_status" != completed ]]; then
        continue
      fi

      if [[ "$run_conclusion" != cancelled ]]; then
        unset "outstanding_pr_run_ids[$run_key]"
        handled_pr_run_ids["$run_key"]=1
        echo "Pull request check ${workflow_file} run ${run_id} completed with ${run_conclusion}; no rerun is needed."
        continue
      fi

      failed_jobs_jq='[.jobs[]? | select(.conclusion == "failure")] | length'
      if [[ "$workflow_file" == ci.yml ]]; then
        failed_jobs_jq='[.jobs[]? | select(.name != "ci" and .conclusion == "failure")] | length'
      fi
      if ! failed_job_count="$(gh api --paginate \
        "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}/jobs?per_page=100" \
        --jq "$failed_jobs_jq" \
        | awk '{ total += $1 } END { print total + 0 }')"; then
        echo "::error::Could not inspect failed jobs for cancelled pull request check ${workflow_file} run ${run_id}; skipping rerun. Issue: $ISSUE"
        recovery_failure_count=$((recovery_failure_count + 1))
        continue
      fi
      if [[ ! "$failed_job_count" =~ ^[0-9]+$ ]]; then
        echo "::error::Could not validate failed job count for cancelled pull request check ${workflow_file} run ${run_id}; skipping rerun. Issue: $ISSUE"
        recovery_failure_count=$((recovery_failure_count + 1))
        continue
      fi
      if (( failed_job_count > 0 )); then
        unset "outstanding_pr_run_ids[$run_key]"
        handled_pr_run_ids["$run_key"]=1
        echo "Skipping cancelled pull request check ${workflow_file} run ${run_id}: it has ${failed_job_count} failed job(s), so the router already handled fail-fast."
        continue
      fi

      if [[ ! "$run_head_sha" =~ ^[[:xdigit:]]{40}$ ]]; then
        unset "outstanding_pr_run_ids[$run_key]"
        handled_pr_run_ids["$run_key"]=1
        echo "::error::Skipping cancelled pull request check ${workflow_file} run ${run_id}: its head SHA is unavailable; rerun it manually if its check remains cancelled. Issue: $ISSUE"
        recovery_failure_count=$((recovery_failure_count + 1))
        continue
      fi

      should_rerun=true
      pull_request_lookup_failed=false
      stale_pull_request_number=""
      stale_pull_request_state=""
      stale_pull_request_head_sha=""
      if [[ "$run_pr_numbers" != - ]]; then
        IFS=',' read -r -a pull_request_numbers <<< "$run_pr_numbers"
        for pull_request_number in "${pull_request_numbers[@]}"; do
          if [[ ! "$pull_request_number" =~ ^[0-9]+$ ]]; then
            echo "::error::Could not validate pull request number for cancelled check ${workflow_file} run ${run_id}; skipping rerun. Issue: $ISSUE"
            recovery_failure_count=$((recovery_failure_count + 1))
            pull_request_lookup_failed=true
            break
          fi
          if ! pull_request_state_and_head="$(gh api \
            "repos/${GITHUB_REPOSITORY}/pulls/${pull_request_number}" \
            --jq '[.state, .head.sha] | @tsv' 2>/dev/null)"; then
            echo "::error::Could not check pull request #${pull_request_number} before re-running ${workflow_file} run ${run_id}; skipping rerun. Issue: $ISSUE"
            recovery_failure_count=$((recovery_failure_count + 1))
            pull_request_lookup_failed=true
            break
          fi
          IFS=$'\t' read -r pull_request_state pull_request_head_sha <<< "$pull_request_state_and_head"
          if [[ "$pull_request_state" != open && "$pull_request_state" != closed ]] || [[ ! "$pull_request_head_sha" =~ ^[[:xdigit:]]{40}$ ]]; then
            echo "::error::Could not validate pull request #${pull_request_number} state or head before re-running ${workflow_file} run ${run_id}; skipping rerun. Issue: $ISSUE"
            recovery_failure_count=$((recovery_failure_count + 1))
            pull_request_lookup_failed=true
            break
          fi
          if [[ "$pull_request_state" != open || "$pull_request_head_sha" != "$run_head_sha" ]]; then
            should_rerun=false
            stale_pull_request_number="$pull_request_number"
            stale_pull_request_state="$pull_request_state"
            stale_pull_request_head_sha="$pull_request_head_sha"
            break
          fi
        done
      fi

      if [[ "$pull_request_lookup_failed" == true ]]; then
        continue
      fi
      if [[ "$should_rerun" != true ]]; then
        unset "outstanding_pr_run_ids[$run_key]"
        handled_pr_run_ids["$run_key"]=1
        echo "Skipping cancelled pull request check ${workflow_file} run ${run_id}: PR #${stale_pull_request_number} is ${stale_pull_request_state} at ${stale_pull_request_head_sha}, not the cancelled run head ${run_head_sha}."
        continue
      fi

      if [[ "${rerun_requested_attempt_by_run[$run_key]:-}" == "$run_attempt" ]]; then
        continue
      fi

      if main_run_id="$(main_ci_active_run_id)"; then
        echo "::notice::Deferring rerun of ${workflow_file} run ${run_id} until main CI run ${main_run_id} completes."
        continue
      else
        main_ci_state_status=$?
        if (( main_ci_state_status != 1 )); then
          echo "::error::Could not check whether main CI is active before re-running ${workflow_file} run ${run_id}; retrying. Issue: $ISSUE"
          recovery_failure_count=$((recovery_failure_count + 1))
          continue
        fi
      fi

      if [[ "$run_pr_numbers" == - ]]; then
        if latest_run_attempt="$(latest_workflow_run_attempt_for_head "$workflow_file" "$run_head_sha")"; then
          IFS=$'\t' read -r latest_run_id latest_run_attempt_number <<< "$latest_run_attempt"
          if [[ "$latest_run_id" != "$run_id" || "$latest_run_attempt_number" != "$run_attempt" ]]; then
            unset "outstanding_pr_run_ids[$run_key]"
            handled_pr_run_ids["$run_key"]=1
            echo "::notice::Skipping cancelled fork check ${workflow_file} run ${run_id}; latest run for head ${run_head_sha} is ${latest_run_id} attempt ${latest_run_attempt_number}."
            continue
          fi
        else
          latest_run_lookup_status=$?
          if (( latest_run_lookup_status != 1 )); then
            echo "::error::Could not confirm the latest ${workflow_file} run for fork head ${run_head_sha}; skipping rerun. Issue: $ISSUE"
          else
            echo "::error::No ${workflow_file} run was found for fork head ${run_head_sha}; skipping rerun. Issue: $ISSUE"
          fi
          recovery_failure_count=$((recovery_failure_count + 1))
          continue
        fi

        if has_open_pull_request_with_head_sha "$run_head_sha"; then
          :
        else
          open_pull_request_lookup_status=$?
          if (( open_pull_request_lookup_status == 1 )); then
            unset "outstanding_pr_run_ids[$run_key]"
            handled_pr_run_ids["$run_key"]=1
            echo "::notice::Skipping cancelled fork check ${workflow_file} run ${run_id}; no open PR has head ${run_head_sha}."
          else
            echo "::error::Could not confirm an open PR still has fork head ${run_head_sha}; retrying ${workflow_file} run ${run_id}. Issue: $ISSUE"
            recovery_failure_count=$((recovery_failure_count + 1))
          fi
          continue
        fi
      fi

      if gh run rerun "$run_id" --repo "$GITHUB_REPOSITORY"; then
        rerun_requested_attempt_by_run["$run_key"]="$run_attempt"
        echo "Re-running cancelled pull request check ${workflow_file} run ${run_id} at attempt ${run_attempt}."
      else
        echo "::error::Could not re-run cancelled pull request check ${workflow_file} run ${run_id}; retrying. Issue: $ISSUE"
        recovery_failure_count=$((recovery_failure_count + 1))
      fi
    done <<< "$runs"
  done

  if ((${#outstanding_pr_run_ids[@]} == 0)); then
    if (( status_check_attempt >= 2 )); then
      break
    fi
    # Let a just-cancelled PR event become visible in the workflow-run index.
    sleep 5
    continue
  fi
  if (( status_check_attempt < max_status_checks )); then
    sleep 60
  fi
done

if ((${#outstanding_pr_run_ids[@]} > 0)); then
  outstanding_run_ids="$(printf '%s\n' "${!outstanding_pr_run_ids[@]}" | LC_ALL=C sort | paste -sd ' ' -)"
  echo "::error::Could not confirm cancelled pull request checks after ${max_status_checks} status checks: ${outstanding_run_ids}; rerun them manually if their checks remain cancelled. Issue: $ISSUE"
  recovery_failure_count=$((recovery_failure_count + 1))
fi

if (( recovery_failure_count > 0 )); then
  echo "::error::Cancelled pull request check recovery had ${recovery_failure_count} error(s). Issue: $ISSUE"
  exit 1
fi
