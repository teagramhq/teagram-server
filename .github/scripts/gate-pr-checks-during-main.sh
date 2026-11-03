#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
pr_number="${PR_NUMBER:-}"
pr_workflow_run_id="${PR_WORKFLOW_RUN_ID:-}"
if [[ -n "$pr_number" && ! "$pr_number" =~ ^[0-9]+$ ]]; then
  echo "::error::PR_NUMBER must be numeric."
  exit 1
fi
if [[ -z "$pr_number" && ! "$pr_workflow_run_id" =~ ^[0-9]+$ ]]; then
  echo "::error::A pull request number or triggering workflow run ID is required."
  exit 1
fi

# shellcheck source=.github/scripts/main-ci-active-run.sh
source "$(dirname "${BASH_SOURCE[0]}")/main-ci-active-run.sh"

if main_run_id="$(main_ci_active_run_id)"; then
  echo "::notice::Deferring pull request checks until main CI run ${main_run_id} completes."
  if [[ -n "$pr_number" ]]; then
    PR_NUMBER="$pr_number" bash "$(dirname "${BASH_SOURCE[0]}")/cancel-pr-checks-for-main.sh"
  elif gh run cancel "$pr_workflow_run_id" --repo "$GITHUB_REPOSITORY"; then
    echo "::notice::Cancelled pull request workflow run ${pr_workflow_run_id}; its pull request number was unavailable."
  else
    if ! run_status="$(gh api \
      "repos/${GITHUB_REPOSITORY}/actions/runs/${pr_workflow_run_id}" \
      --jq '.status' 2>/dev/null)"; then
      echo "::error::Could not cancel pull request workflow run ${pr_workflow_run_id} or confirm its status."
      exit 1
    fi
    if [[ "$run_status" == completed ]]; then
      echo "::notice::Pull request workflow run ${pr_workflow_run_id} completed before it could be cancelled."
    else
      echo "::error::Could not cancel pull request workflow run ${pr_workflow_run_id} (status: ${run_status})."
      exit 1
    fi
  fi
  exit 0
else
  state_status=$?
  if (( state_status != 1 )); then
    echo "::error::Could not check whether main CI is active; blocking this pull request run."
    exit 1
  fi
fi

echo "No active main ci aggregate; pull request checks may proceed."
