#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${MAIN_RUN_ID:?MAIN_RUN_ID is required}"
: "${MAIN_RUN_ATTEMPT:?MAIN_RUN_ATTEMPT is required}"
: "${MAIN_HEAD_SHA:?MAIN_HEAD_SHA is required}"

if [[ ! "$MAIN_RUN_ID" =~ ^[0-9]+$ || ! "$MAIN_RUN_ATTEMPT" =~ ^[0-9]+$ || ! "$MAIN_HEAD_SHA" =~ ^[[:xdigit:]]{40}$ ]]; then
  echo "::error::Invalid main workflow identity; cannot wait for trusted PR cancellation."
  exit 1
fi

max_status_checks="${PRIORITY_MAX_STATUS_CHECKS:-31}"
status_check_interval="${PRIORITY_STATUS_CHECK_INTERVAL_SECONDS:-10}"
if [[ ! "$max_status_checks" =~ ^[0-9]+$ ]] || (( max_status_checks < 1 )); then
  echo "::error::Invalid main-priority status check limit."
  exit 1
fi
if [[ ! "$status_check_interval" =~ ^[0-9]+$ ]]; then
  echo "::error::Invalid main-priority status check interval."
  exit 1
fi

status_context="main-priority-${MAIN_RUN_ID}-${MAIN_RUN_ATTEMPT}"
warn_and_continue() {
  echo "::warning::$1 Main CI fan-out will proceed without confirmed PR cancellation for run ${MAIN_RUN_ID}."
  exit 0
}

for ((attempt = 1; attempt <= max_status_checks; attempt++)); do
  if ! priority_status="$(gh api \
    "repos/${GITHUB_REPOSITORY}/commits/${MAIN_HEAD_SHA}/statuses" \
    --jq "[.[] | select(.context == \"${status_context}\")][0].state // \"\"" 2>/dev/null)"; then
    warn_and_continue "Could not read the trusted main-priority status for run ${MAIN_RUN_ID}."
  fi

  case "$priority_status" in
    success)
      echo "Trusted PR cancellation completed for main CI run ${MAIN_RUN_ID}."
      exit 0
      ;;
    failure|error)
      warn_and_continue "Trusted PR cancellation reported ${priority_status} for main CI run ${MAIN_RUN_ID}."
      ;;
    pending|"")
      ;;
    *)
      warn_and_continue "Unexpected main-priority status '${priority_status}' for run ${MAIN_RUN_ID}."
      ;;
  esac

  if (( attempt < max_status_checks )); then
    sleep "$status_check_interval"
  fi
done

warn_and_continue "Timed out waiting for trusted PR cancellation before main CI fan-out for run ${MAIN_RUN_ID}."
