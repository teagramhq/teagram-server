#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

cancelled_pr_run_targets=""
publish_cancelled_pr_run_targets() {
  printf 'cancelled_pr_run_targets=%s\n' "${cancelled_pr_run_targets% }" >> "$GITHUB_OUTPUT"
}

workflow_files=(ci.yml perf.yml real-client-browser-flow.yml)
for workflow_file in "${workflow_files[@]}"; do
  for run_status_filter in queued in_progress pending waiting requested; do
    if ! runs="$(gh api --paginate \
      "repos/${GITHUB_REPOSITORY}/actions/workflows/${workflow_file}/runs?event=pull_request&status=${run_status_filter}&per_page=100" \
      --jq '.workflow_runs[]? | [.id, .created_at] | @tsv' 2>/dev/null)"; then
      echo "::warning::Could not list ${run_status_filter} pull request runs for ${workflow_file}; continuing."
      continue
    fi

    while IFS=$'\t' read -r run_id run_created_at; do
      [[ "$run_id" =~ ^[0-9]+$ ]] || continue
      [[ "$run_created_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$ ]] || {
        echo "::warning::Ignoring ${workflow_file} run ${run_id} with an invalid creation timestamp."
        continue
      }

      if gh run cancel "$run_id" --repo "$GITHUB_REPOSITORY"; then
        cancelled_pr_run_targets+="${workflow_file}:${run_id}:${run_created_at} "
      else
        echo "::warning::Could not cancel pull request run ${run_id} in ${workflow_file}; it may have completed or been superseded."
      fi
    done <<< "$runs"
  done
done

publish_cancelled_pr_run_targets
