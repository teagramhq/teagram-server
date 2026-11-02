#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${MAIN_RUN_ID:?MAIN_RUN_ID is required}"
: "${MAIN_RUN_ATTEMPT:?MAIN_RUN_ATTEMPT is required}"
: "${MAIN_HEAD_SHA:?MAIN_HEAD_SHA is required}"
: "${ISSUE:?ISSUE is required}"

if [[ ! "$MAIN_RUN_ID" =~ ^[0-9]+$ || ! "$MAIN_RUN_ATTEMPT" =~ ^[0-9]+$ || ! "$MAIN_HEAD_SHA" =~ ^[[:xdigit:]]{40}$ ]]; then
  echo "::error::Invalid main workflow identity; cannot dispatch PR check recovery."
  exit 1
fi

if ! main_ci_job_ids="$(gh api --paginate \
  "repos/${GITHUB_REPOSITORY}/actions/runs/${MAIN_RUN_ID}/jobs?per_page=100" \
  --jq '.jobs[]? | select(.name == "ci") | .id' 2>/dev/null)"; then
  echo "::error::Could not inspect CI jobs for main run ${MAIN_RUN_ID}; cannot dispatch PR check recovery. Issue: $ISSUE"
  exit 1
fi
if [[ -z "$main_ci_job_ids" ]]; then
  echo "::notice::Main CI run ${MAIN_RUN_ID} never started and has no ci job; skipping PR check recovery."
  exit 0
fi

artifact_name="cancelled-pr-run-targets-${MAIN_RUN_ID}-${MAIN_RUN_ATTEMPT}"
if ! artifacts="$(gh api --paginate \
  "repos/${GITHUB_REPOSITORY}/actions/artifacts?name=${artifact_name}&per_page=100" \
  --jq '.artifacts[]? | select(.expired == false) | [.name, (.workflow_run.id | tostring)] | @tsv')"; then
  echo "::error::Could not list the cancellation manifest artifact for main CI run ${MAIN_RUN_ID}. Issue: $ISSUE"
  exit 1
fi

priority_run_id=""
artifact_match_count=0
while IFS=$'\t' read -r listed_artifact_name listed_priority_run_id; do
  [[ "$listed_artifact_name" == "$artifact_name" ]] || continue
  if [[ ! "$listed_priority_run_id" =~ ^[0-9]+$ ]]; then
    echo "::error::The cancellation manifest artifact has no valid workflow run ID. Issue: $ISSUE"
    exit 1
  fi
  if ! priority_run_metadata="$(gh api \
    "repos/${GITHUB_REPOSITORY}/actions/runs/${listed_priority_run_id}" \
    --jq '[.name, .event, .head_branch] | @tsv')"; then
    echo "::error::Could not validate the cancellation manifest's workflow run. Issue: $ISSUE"
    exit 1
  fi
  IFS=$'\t' read -r priority_run_name priority_run_event priority_run_branch <<< "$priority_run_metadata"
  if [[ "$priority_run_name" != 'PR CI priority' || "$priority_run_event" != workflow_run || "$priority_run_branch" != main ]]; then
    continue
  fi
  priority_run_id="$listed_priority_run_id"
  artifact_match_count=$((artifact_match_count + 1))
done <<< "$artifacts"

if (( artifact_match_count != 1 )); then
  echo "::error::Expected one cancellation manifest for main CI run ${MAIN_RUN_ID}, found ${artifact_match_count}. Issue: $ISSUE"
  exit 1
fi

artifact_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/cancelled-pr-targets.XXXXXX")"
trap 'rm -rf -- "$artifact_dir"' EXIT
if ! gh run download "$priority_run_id" \
  --name "$artifact_name" \
  --dir "$artifact_dir" \
  --repo "$GITHUB_REPOSITORY"; then
  echo "::error::Could not download the cancellation manifest for main CI run ${MAIN_RUN_ID}. Issue: $ISSUE"
  exit 1
fi

targets_file="$artifact_dir/cancelled-pr-run-targets.txt"
if [[ ! -f "$targets_file" ]]; then
  echo "::error::The cancellation manifest file is missing for main CI run ${MAIN_RUN_ID}. Issue: $ISSUE"
  exit 1
fi
cancelled_pr_run_targets="$(<"$targets_file")"
if [[ -n "$cancelled_pr_run_targets" ]]; then
  read -r -a targets <<< "$cancelled_pr_run_targets"
  for target in "${targets[@]}"; do
    if [[ ! "$target" =~ ^(ci\.yml|perf\.yml|real-client-browser-flow\.yml):([0-9]+):([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z)$ ]]; then
      echo "::error::The cancellation manifest contains an invalid PR run target. Issue: $ISSUE"
      exit 1
    fi
  done
fi

gh workflow run recover-cancelled-pr-ci.yml \
  --repo "$GITHUB_REPOSITORY" \
  --ref main \
  -f "issue=$ISSUE" \
  -f "main_run_id=$MAIN_RUN_ID" \
  -f "cancelled_pr_run_targets=$cancelled_pr_run_targets"
