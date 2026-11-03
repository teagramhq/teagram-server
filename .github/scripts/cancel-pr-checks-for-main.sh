#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

if [[ -n "${MAIN_RUN_ID:-}" && ! "$MAIN_RUN_ID" =~ ^[0-9]+$ ]]; then
  echo "::error::MAIN_RUN_ID must be numeric."
  exit 1
fi

if [[ -n "${PR_NUMBER:-}" && ! "$PR_NUMBER" =~ ^[0-9]+$ ]]; then
  echo "::error::PR_NUMBER must be numeric when filtering pull request runs."
  exit 1
fi

cancelled_pr_run_targets=""
main_ci_completed=false
publish_cancelled_pr_run_targets() {
  printf 'cancelled_pr_run_targets=%s\n' "${cancelled_pr_run_targets% }" >> "$GITHUB_OUTPUT"
  printf 'main_ci_completed=%s\n' "$main_ci_completed" >> "$GITHUB_OUTPUT"
}
trap publish_cancelled_pr_run_targets EXIT

main_ci_job_is_complete() {
  local ci_job_statuses ci_job_status

  [[ -n "${MAIN_RUN_ID:-}" ]] || return 1
  if ! ci_job_statuses="$(gh api --paginate \
    "repos/${GITHUB_REPOSITORY}/actions/runs/${MAIN_RUN_ID}/jobs?per_page=100" \
    --jq '.jobs[]? | select(.name == "ci") | .status' 2>/dev/null)"; then
    return 2
  fi
  while IFS= read -r ci_job_status; do
    [[ "$ci_job_status" == completed ]] && return 0
  done <<< "$ci_job_statuses"
  return 1
}

if [[ -n "${MAIN_RUN_ID:-}" ]]; then
  if main_ci_job_is_complete; then
    main_ci_completed=true
    echo "::notice::Main CI run ${MAIN_RUN_ID} ci job is already complete; skipping cancellation sweep."
    exit 0
  else
    main_ci_status=$?
    if (( main_ci_status != 1 )); then
      echo "::error::Could not confirm whether main CI run ${MAIN_RUN_ID} is complete; main CI priority cannot be enforced."
      exit 1
    fi
  fi
fi

workflow_files=(ci.yml perf.yml real-client-browser-flow.yml)
for workflow_file in "${workflow_files[@]}"; do
  for run_status_filter in queued in_progress pending waiting requested; do
    if ! runs="$(gh api --paginate \
      "repos/${GITHUB_REPOSITORY}/actions/workflows/${workflow_file}/runs?event=pull_request&status=${run_status_filter}&per_page=100" \
      --jq '.workflow_runs[]? | [.id, .created_at, ([.pull_requests // [] | .[] | .number] | join(","))] | @tsv' 2>/dev/null)"; then
      echo "::error::Could not list ${run_status_filter} pull request runs for ${workflow_file}; main CI priority cannot be enforced."
      exit 1
    fi

    while IFS=$'\t' read -r run_id run_created_at run_pr_numbers; do
      [[ "$run_id" =~ ^[0-9]+$ ]] || continue
      [[ "$run_created_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$ ]] || {
        echo "::error::Could not validate creation time for ${workflow_file} run ${run_id}; main CI priority cannot be enforced."
        exit 1
      }
      if [[ -n "${PR_NUMBER:-}" && ",$run_pr_numbers," != *",$PR_NUMBER,"* ]]; then
        continue
      fi

      if [[ -n "${MAIN_RUN_ID:-}" ]]; then
        if main_ci_job_is_complete; then
          main_ci_completed=true
          echo "::notice::Main CI run ${MAIN_RUN_ID} ci job completed during the cancellation sweep; stopping PR cancellation."
          break 3
        else
          main_ci_status=$?
          if (( main_ci_status != 1 )); then
            echo "::error::Could not confirm whether main CI run ${MAIN_RUN_ID} is complete; main CI priority cannot be enforced."
            exit 1
          fi
        fi
      fi

      if gh run cancel "$run_id" --repo "$GITHUB_REPOSITORY"; then
        cancelled_pr_run_targets+="${workflow_file}:${run_id}:${run_created_at} "
      else
        if ! run_status_and_conclusion="$(gh api \
          "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}" \
          --jq '[.status, (.conclusion // "")] | @tsv' 2>/dev/null)"; then
          echo "::error::Could not cancel pull request run ${run_id} in ${workflow_file} or confirm its status and conclusion; main CI priority cannot be enforced."
          exit 1
        fi
        IFS=$'\t' read -r run_status run_conclusion <<< "$run_status_and_conclusion"
        if [[ "$run_status" == completed ]]; then
          if [[ "$run_conclusion" == cancelled ]]; then
            cancelled_pr_run_targets+="${workflow_file}:${run_id}:${run_created_at} "
            echo "::notice::Pull request run ${run_id} in ${workflow_file} was already cancelled; including it for recovery."
            continue
          fi
          echo "::notice::Pull request run ${run_id} in ${workflow_file} completed before it could be cancelled."
          continue
        fi
        echo "::error::Could not cancel pull request run ${run_id} in ${workflow_file} (status: ${run_status}); main CI priority cannot be enforced."
        exit 1
      fi
    done <<< "$runs"
  done
done
