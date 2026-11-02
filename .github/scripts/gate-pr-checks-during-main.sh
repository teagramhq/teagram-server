#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"

source "$(dirname "${BASH_SOURCE[0]}")/main-ci-active-run.sh"

if main_run_id="$(main_ci_active_run_id)"; then
  echo "::notice::Deferring pull request checks until main CI run ${main_run_id} completes."
  if ! gh run cancel "$GITHUB_RUN_ID" --repo "$GITHUB_REPOSITORY"; then
    echo "::error::Could not defer pull request checks while main CI run ${main_run_id} is active."
    exit 1
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
