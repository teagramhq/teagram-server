#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"

if ! main_run_ids="$(gh api \
  "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs?branch=main&event=push&per_page=100" \
  --jq '.workflow_runs[]? | select(.status != "completed") | .id' 2>/dev/null)"; then
  echo "::error::Could not check whether main CI is active; blocking this pull request run."
  exit 1
fi

while IFS= read -r main_run_id; do
  [[ "$main_run_id" =~ ^[0-9]+$ ]] || continue
  if ! ci_job_status="$(gh api --paginate \
    "repos/${GITHUB_REPOSITORY}/actions/runs/${main_run_id}/jobs?per_page=100" \
    --jq '.jobs[]? | select(.name == "ci") | .status' 2>/dev/null)"; then
    echo "::error::Could not check the ci aggregate for main run ${main_run_id}; blocking this pull request run."
    exit 1
  fi

  if [[ "$ci_job_status" == completed ]]; then
    continue
  fi

  echo "::notice::Deferring pull request checks until main CI run ${main_run_id} completes."
  gh run cancel "$GITHUB_RUN_ID" --repo "$GITHUB_REPOSITORY"
  exit 0
done <<< "$main_run_ids"

echo "No active main ci aggregate; pull request checks may proceed."
