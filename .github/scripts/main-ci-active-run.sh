#!/usr/bin/env bash

# Return the first main workflow run whose required CI aggregate is incomplete.
# Exit 1 means main is idle; exit 2 means the state could not be checked.
main_ci_active_run_id() {
  local main_run_ids main_run_id ci_job_status

  if ! main_run_ids="$(gh api \
    "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=100" \
    --jq '.workflow_runs[]? | select(.status != "completed") | .id' 2>/dev/null)"; then
    return 2
  fi

  while IFS= read -r main_run_id; do
    [[ "$main_run_id" =~ ^[0-9]+$ ]] || continue
    if ! ci_job_status="$(gh api --paginate \
      "repos/${GITHUB_REPOSITORY}/actions/runs/${main_run_id}/jobs?per_page=100" \
      --jq '.jobs[]? | select(.name == "ci") | .status' 2>/dev/null)"; then
      return 2
    fi

    if [[ "$ci_job_status" != completed ]]; then
      printf '%s\n' "$main_run_id"
      return 0
    fi
  done <<< "$main_run_ids"

  return 1
}
