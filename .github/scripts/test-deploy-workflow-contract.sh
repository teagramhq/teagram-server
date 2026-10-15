#!/usr/bin/env bash
# Guards the production deploy workflow: it may run only after CI passes on a
# push to main (or by manual dispatch), one at a time, in the production
# environment, without checking out any code.
set -euo pipefail

workflow="${DEPLOY_WORKFLOW:-.github/workflows/deploy.yml}"
expected_if="\${{ github.event_name == 'workflow_dispatch' || (github.event.workflow_run.conclusion == 'success' && github.event.workflow_run.event == 'push') }}"

fail() {
  printf '%s: %s\n' "$workflow" "$1" >&2
  exit 1
}

query() {
  yq -r "$1" "$workflow"
}

check_contract() {
  [[ "$(query '.on | keys | sort | join(",")')" == 'workflow_dispatch,workflow_run' ]] ||
    fail 'triggers must be exactly workflow_run and workflow_dispatch'
  [[ "$(query '.on.workflow_dispatch // {} | length')" == 0 ]] ||
    fail 'workflow_dispatch must take no inputs'
  [[ "$(query '.on.workflow_run.workflows | join(",")')" == 'CI' ]] ||
    fail 'workflow_run must follow only the CI workflow'
  [[ "$(query '.on.workflow_run.types | join(",")')" == 'completed' ]] ||
    fail 'workflow_run must fire only on completed'
  [[ "$(query '.on.workflow_run.branches | join(",")')" == 'main' ]] ||
    fail 'workflow_run must be limited to branches: [main]'
  [[ "$(query '.concurrency.group')" == 'deploy-telegram-server' ]] ||
    fail 'concurrency group must be deploy-telegram-server'
  [[ "$(query '.concurrency."cancel-in-progress"')" == 'false' ]] ||
    fail 'concurrency must not cancel a deploy in progress'
  [[ "$(query '.permissions | to_entries | map(.key + "=" + .value) | join(",")')" == 'contents=read' ]] ||
    fail 'workflow permissions must be contents: read only'

  local jobs
  jobs="$(query '.jobs | keys | .[]')"
  [[ -n "$jobs" ]] || fail 'no jobs'
  local job
  for job in $jobs; do
    [[ "$(query ".jobs.\"$job\".if")" == "$expected_if" ]] ||
      fail "job $job must run only on dispatch or a successful CI push run"
    [[ "$(query ".jobs.\"$job\".environment")" == 'production' ]] ||
      fail "job $job must use environment: production"
    [[ "$(query ".jobs.\"$job\".permissions // {} | to_entries | map(select(.value != \"read\")) | length")" == 0 ]] ||
      fail "job $job must not request write permissions"
  done

  local uses
  while IFS= read -r uses; do
    [[ -n "$uses" ]] || continue
    case "$uses" in
      actions/checkout@*) fail 'must not use actions/checkout' ;;
    esac
    [[ "$uses" =~ ^[^@]+@[0-9a-f]{40}$ ]] || fail "action not pinned to a commit SHA: $uses"
  done < <(query '.jobs[].steps[].uses // ""')
}

# Each mutation breaks one rule; the contract must reject all of them.
self_test() {
  local tmp mutation
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  # shellcheck disable=SC2016 # literal workflow expressions
  local mutations=(
    '.on.pull_request = {}'
    '.on.pull_request_target = {}'
    '.on.push = {"branches": ["main"]}'
    'del(.on.workflow_run.branches)'
    '.on.workflow_run.branches = ["main", "dev"]'
    '.jobs.deploy.if = "${{ github.event.workflow_run.event == '"'push'"' }}"'
    '.jobs.deploy.if = "${{ github.event.workflow_run.conclusion == '"'success'"' }}"'
    'del(.jobs.deploy.if)'
    'del(.jobs.deploy.environment)'
    'del(.concurrency)'
    '.concurrency.group = "deploy"'
    '.concurrency."cancel-in-progress" = true'
    '.jobs.deploy.steps = [{"uses": "actions/checkout@08eba0b27e820071cde6df949e0beb9ba4906955"}] + .jobs.deploy.steps'
    '.jobs.deploy.steps[0].uses = "tailscale/github-action@v4"'
    '.permissions.contents = "write"'
  )
  for mutation in "${mutations[@]}"; do
    yq "$mutation" "$workflow" > "$tmp/deploy.yml"
    if DEPLOY_WORKFLOW="$tmp/deploy.yml" "$0" --contract-only 2>/dev/null; then
      printf 'contract accepted a broken workflow: %s\n' "$mutation" >&2
      exit 1
    fi
  done
}

check_contract
if [[ "${1:-}" != '--contract-only' ]]; then
  self_test
  printf 'deploy workflow contract holds\n'
fi
