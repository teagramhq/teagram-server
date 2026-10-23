#!/usr/bin/env bash
# Guards the production deploy workflow: it may run only after CI passes on a
# push to main (or by manual dispatch), one at a time, in the production
# environment, without checking out any code, and it deploys the commit whose
# CI run passed.
# shellcheck disable=SC2016 # single-quoted strings are literal workflow text
set -euo pipefail

workflow="${DEPLOY_WORKFLOW:-.github/workflows/deploy.yml}"
ci_workflow="${CI_WORKFLOW:-.github/workflows/ci.yml}"
expected_if="\${{ github.event_name == 'workflow_dispatch' || (github.event.workflow_run.conclusion == 'success' && github.event.workflow_run.event == 'push') }}"
expected_demo_if="\${{ needs.deploy.result == 'success' && vars.TEAGRAM_DEMO_DEPLOY_ENABLED == 'true' }}"
expected_ci_group="\${{ github.workflow }}-\${{ github.event_name == 'pull_request' && format('pr-{0}', github.event.pull_request.number) || (github.event_name == 'push' && github.ref == 'refs/heads/main' && 'main') || format('run-{0}-{1}', github.run_id, github.run_attempt) }}"
expected_ci_cancel="\${{ github.event_name == 'pull_request' }}"
expected_run_name="\${{ inputs.issue && format('{0} [{1}]', github.workflow, inputs.issue) || '' }}"
expected_issue_description='Issue key to report this run to (e.g. MAIN-123). Leave empty for no report.'

fail() {
  printf '%s: %s\n' "$workflow" "$1" >&2
  exit 1
}

query() {
  yq -r "$1" "$workflow"
}

ci_fail() {
  printf '%s: %s\n' "$ci_workflow" "$1" >&2
  exit 1
}

ci_query() {
  yq -r "$1" "$ci_workflow"
}

check_ci_contract() {
  [[ "$(ci_query '.on | keys | sort | join(",")')" == 'pull_request,push' ]] ||
    ci_fail 'triggers must be exactly push and pull_request'
  [[ "$(ci_query '.on.push.branches | join(",")')" == 'main' ]] ||
    ci_fail 'push CI must be limited to main'
  [[ "$(ci_query '.concurrency.group')" == "$expected_ci_group" ]] ||
    ci_fail 'main pushes must share a group, PR updates must keep per-PR groups, and other runs must stay isolated'
  [[ "$(ci_query '.concurrency."cancel-in-progress"')" == "$expected_ci_cancel" ]] ||
    ci_fail 'only pull request updates may cancel in-progress CI runs'
}

# Run the actual target step against controlled forge responses.
check_target_selection() (
  set -euo pipefail
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  query '.jobs[].steps[] | select(.id == "target") | .run' > "$tmp/target.sh"
  cat > "$tmp/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'api repos/test/server/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=1 --jq .workflow_runs[0].head_sha')
    printf '%s\n' "$TEST_LATEST_SHA"
    ;;
  "api repos/test/server/compare/$RUN_HEAD_SHA...$TEST_LATEST_SHA --jq .status")
    [[ "$TEST_COMPARISON" != api-error ]] || exit 1
    printf '%s\n' "$TEST_COMPARISON"
    ;;
  *) echo "unexpected forge query: $*" >&2; exit 1 ;;
esac
SH
  chmod +x "$tmp/gh"
  export PATH="$tmp:$PATH" GITHUB_REPOSITORY=test/server
  export RUN_HEAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  export TEST_LATEST_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  export GITHUB_OUTPUT="$tmp/output" TEST_COMPARISON GITHUB_EVENT_NAME
  local expected status
  GITHUB_EVENT_NAME=workflow_run
  for status in ahead identical behind diverged; do
    TEST_COMPARISON="$status"
    TEST_LATEST_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    if [[ "$status" == identical ]]; then
      TEST_LATEST_SHA="$RUN_HEAD_SHA"
    fi
    expected="$RUN_HEAD_SHA"
    if [[ "$status" == ahead || "$status" == identical ]]; then
      expected="$TEST_LATEST_SHA"
    fi
    : > "$GITHUB_OUTPUT"
    bash -euo pipefail "$tmp/target.sh" > /dev/null
    [[ "$(cat "$GITHUB_OUTPUT")" == "sha=$expected" ]] ||
      fail "workflow_run must select a successful descendant and otherwise retain head_sha ($status)"
  done
  TEST_COMPARISON=api-error
  if bash -euo pipefail "$tmp/target.sh" > /dev/null; then
    fail 'a failed forge comparison must fail the deploy'
  fi
  GITHUB_EVENT_NAME=workflow_dispatch
  RUN_HEAD_SHA=''
  : > "$GITHUB_OUTPUT"
  bash -euo pipefail "$tmp/target.sh" > /dev/null
  [[ "$(cat "$GITHUB_OUTPUT")" == "sha=$TEST_LATEST_SHA" ]] ||
    fail 'manual dispatch must select the latest successful CI push head'
  TEST_LATEST_SHA=null
  if bash -euo pipefail "$tmp/target.sh" > /dev/null; then
    fail 'an invalid successful CI head must fail the deploy'
  fi
)

check_contract() {
  [[ "$(query '.on | keys | sort | join(",")')" == 'workflow_dispatch,workflow_run' ]] ||
    fail 'triggers must be exactly workflow_run and workflow_dispatch'
  [[ "$(query '.on.workflow_dispatch.inputs | keys | join(",")')" == 'issue' ]] ||
    fail 'workflow_dispatch must take only the optional issue input'
  [[ "$(query '.on.workflow_dispatch.inputs.issue.description')" == "$expected_issue_description" ]] ||
    fail 'the issue input description must explain optional reporting'
  [[ "$(query '.on.workflow_dispatch.inputs.issue.required')" == 'false' ]] ||
    fail 'the issue input must be optional'
  [[ "$(query '.on.workflow_dispatch.inputs.issue.type')" == 'string' ]] ||
    fail 'the issue input must be a string'
  [[ -z "$(query '.on.workflow_dispatch.inputs.issue.default')" ]] ||
    fail 'the issue input must default to empty'
  [[ "$(query '."run-name"')" == "$expected_run_name" ]] ||
    fail 'run names must include only non-empty issue keys'
  [[ "$(query '.on.workflow_run.workflows | join(",")')" == 'CI' ]] ||
    fail 'workflow_run must follow only the CI workflow'
  [[ "$(query '.on.workflow_run.types | join(",")')" == 'completed' ]] ||
    fail 'workflow_run must fire only on completed'
  [[ "$(query '.on.workflow_run.branches | join(",")')" == 'main' ]] ||
    fail 'workflow_run must be limited to branches: [main]'
  [[ "$(query '.concurrency.group')" == 'deploy-teagram-server' ]] ||
    fail 'concurrency group must be deploy-teagram-server'
  [[ "$(query '.concurrency."cancel-in-progress"')" == 'false' ]] ||
    fail 'concurrency must not cancel a deploy in progress'
  [[ "$(query '.permissions | to_entries | map(.key + "=" + .value) | join(",")')" == 'contents=read' ]] ||
    fail 'workflow permissions must be contents: read only'

  [[ "$(query '.jobs | keys | sort | join(",")')" == 'deploy,deploy-demo' ]] ||
    fail 'jobs must contain only the dev deploy and gated demo deploy'
  [[ "$(query '.jobs.deploy.if')" == "$expected_if" ]] ||
    fail 'the dev job must run only on dispatch or a successful CI push run'
  [[ "$(query '.jobs.deploy.environment')" == 'production' ]] ||
    fail 'the dev job must use environment: production'
  [[ "$(query '.jobs.deploy.permissions // {} | to_entries | map(select(.value != "read")) | length')" == 0 ]] ||
    fail 'the dev job must not request write permissions'
  [[ "$(query '.jobs.deploy.permissions.actions')" == 'read' ]] ||
    fail 'the dev job must grant actions: read to resolve successful CI runs'
  [[ "$(query '.jobs.deploy.outputs.sha')" == '${{ steps.target.outputs.sha }}' ]] ||
    fail 'the dev job must expose the tested SHA to the demo job'
  [[ "$(query '.jobs.deploy-demo.needs')" == 'deploy' ]] ||
    fail 'the demo job must wait for a successful dev deployment'
  [[ "$(query '.jobs.deploy-demo.if')" == "$expected_demo_if" ]] ||
    fail 'the demo job must require both a successful dev job and the explicit feature gate'
  [[ "$(query '.jobs.deploy-demo.environment')" == 'demo' ]] ||
    fail 'the demo job must use the separate demo environment'
  [[ "$(query '.jobs.deploy-demo.permissions // {} | to_entries | map(select(.value != "read")) | length')" == 0 ]] ||
    fail 'the demo job must not request write permissions'

  [[ "$(query '.jobs[].steps[] | select(.id == "target") | .env.RUN_HEAD_SHA')" == '${{ github.event.workflow_run.head_sha }}' ]] ||
    fail 'the target step must take the CI run head_sha'
  query '.jobs[].steps[] | select(.id == "target") | .run' |
    grep -Fq 'actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=1' ||
    fail 'a manual run must resolve the newest successful CI push run on main'
  local ssh_steps demo_ssh_steps demo_port_run
  ssh_steps="$(query '[.jobs.deploy.steps[] | select(.run // "" | test("(^|\s)ssh\s"))] | length')"
  [[ "$ssh_steps" == 1 ]] || fail "expected one dev ssh step, found $ssh_steps"
  [[ "$(query '.jobs.deploy.steps[] | select(.run // "" | test("(^|\s)ssh\s")) | .env.DEPLOY_SHA')" == '${{ steps.target.outputs.sha }}' ]] ||
    fail 'the dev ssh step must take DEPLOY_SHA from the target step'
  query '.jobs.deploy.steps[] | select(.run // "" | test("(^|\s)ssh\s")) | .run' |
    grep -Fq 'root@100.124.236.66 "$DEPLOY_SHA"' ||
    fail 'the dev ssh step must send DEPLOY_SHA as its command'
  demo_ssh_steps="$(query '[.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\s)ssh\s"))] | length')"
  [[ "$demo_ssh_steps" == 1 ]] || fail "expected one demo ssh step, found $demo_ssh_steps"
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\s)ssh\s")) | .env.DEPLOY_SHA')" == '${{ needs.deploy.outputs.sha }}' ]] ||
    fail 'the demo ssh step must deploy the exact SHA from the dev job'
  query '.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\s)ssh\s")) | .run' |
    grep -Fq '"root@$DEMO_DEPLOY_HOST" "$DEPLOY_SHA"' ||
    fail 'the demo ssh step must send the tested SHA to the separate demo host'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Install the demo deploy key") | .env.DEMO_DEPLOY_PORT')" == '${{ vars.DEMO_DEPLOY_PORT }}' ]] ||
    fail 'the demo key step must validate the configured SSH port'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\\s)ssh\\s")) | .env.DEMO_DEPLOY_PORT')" == '${{ vars.DEMO_DEPLOY_PORT }}' ]] ||
    fail 'the demo SSH step must use the configured SSH port'
  query '.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\\s)ssh\\s")) | .run' |
    grep -Fq 'ssh -T -p "$DEMO_DEPLOY_PORT"' || fail 'demo SSH must pass the non-default port'
  demo_port_run="$(query '.jobs.deploy-demo.steps[] | select(.name == "Install the demo deploy key") | .run')"
  grep -Fq '[[ "$DEMO_DEPLOY_PORT" =~ ^[0-9]{1,5}$ ]]' <<< "$demo_port_run" ||
    fail 'demo SSH port must contain only decimal digits'
  grep -Fq '(( 10#$DEMO_DEPLOY_PORT >= 1 && 10#$DEMO_DEPLOY_PORT <= 65535 ))' <<< "$demo_port_run" ||
    fail 'demo SSH port must be within 1..65535'
  query '.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\s)ssh\s")) | .run' |
    grep -Fq 'StrictHostKeyChecking=yes' || fail 'the demo ssh step must verify the host key'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Install the demo deploy key") | .env.DEMO_DEPLOY_SSH_KEY')" == '${{ secrets.DEMO_DEPLOY_SSH_KEY }}' ]] ||
    fail 'the demo job must use its environment-scoped deploy key'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Install the demo deploy key") | .env.DEMO_DEPLOY_KNOWN_HOSTS')" == '${{ vars.DEMO_DEPLOY_KNOWN_HOSTS }}' ]] ||
    fail 'the demo job must use its pinned known-hosts value'
  [[ "$(query '[.jobs[].steps[].run // "" | select(test("\$\{\{"))] | length')" == 0 ]] ||
    fail 'run blocks must not interpolate ${{ }}; pass values through env'

  local uses
  while IFS= read -r uses; do
    [[ -n "$uses" ]] || continue
    case "$uses" in
      actions/checkout@*) fail 'must not use actions/checkout' ;;
    esac
    [[ "$uses" =~ ^[^@]+@[0-9a-f]{40}$ ]] || fail "action not pinned to a commit SHA: $uses"
  done < <(query '.jobs[].steps[].uses // ""')
  check_target_selection
}

# Each mutation breaks one rule; the contract must reject all of them.
self_test() {
  local tmp mutation
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  local mutations=(
    '.on.pull_request = {}'
    '.on.pull_request_target = {}'
    '.on.push = {"branches": ["main"]}'
    '.on.workflow_dispatch.inputs.issue.required = true'
    '.on.workflow_dispatch.inputs.extra = .on.workflow_dispatch.inputs.issue'
    'del(."run-name")'
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
    '(.jobs.deploy.steps[] | select(.uses) | .uses) = "tailscale/github-action@v4"'
    '.permissions.contents = "write"'
    'del(.jobs.deploy.permissions.actions)'
    '(.jobs.deploy.steps[] | select(.id == "target") | .run) |= sub("== workflow_run"; "== disabled")'
    '(.jobs.deploy.steps[] | select(.id == "target") | .run) |= sub("comparison\" == ahead"; "comparison\" == behind")'
    '(.jobs.deploy.steps[] | select(.id == "target") | .run) |= sub("compare/\$sha...\$latest"; "compare/$latest...$sha")'
    '(.jobs.deploy.steps[] | select(.id == "target") | .run) |= sub("comparison\" == identical"; "comparison\" == diverged")'
    '(.jobs.deploy.steps[] | select(.id == "target") | .env.RUN_HEAD_SHA) = "${{ github.sha }}"'
    '(.jobs.deploy.steps[] | select(.id == "target") | .run) |= sub("event=push&"; "")'
    '(.jobs.deploy.steps[] | select(.env.DEPLOY_SHA) | .run) |= sub(" \"\$DEPLOY_SHA\""; "")'
    '(.jobs.deploy.steps[] | select(.env.DEPLOY_SHA) | .env.DEPLOY_SHA) = "${{ github.sha }}"'
    '(.jobs.deploy.steps[] | select(.env.DEPLOY_SHA) | .run) |= sub("\"\$DEPLOY_SHA\""; "${{ steps.target.outputs.sha }}")'
    '(.jobs.deploy.steps[] | select(.id == "target") | .run) |= sub("deploying \$sha"; "deploying ${{ github.sha }}")'
  )
  for mutation in "${mutations[@]}"; do
    yq "$mutation" "$workflow" > "$tmp/deploy.yml"
    if DEPLOY_WORKFLOW="$tmp/deploy.yml" "$0" --contract-only 2>/dev/null; then
      printf 'contract accepted a broken workflow: %s\n' "$mutation" >&2
      exit 1
    fi
  done

  local demo_mutations=(
    'del(.jobs."deploy-demo")'
    '.jobs."deploy-demo".needs = []'
    '.jobs."deploy-demo".if = "${{ github.event_name == '"'"'workflow_dispatch'"'"' }}"'
    '.jobs."deploy-demo".environment = "production"'
    '(.jobs."deploy-demo".steps[] | select(.name == "Install the demo deploy key") | .env.DEMO_DEPLOY_SSH_KEY) = "${{ secrets.DEPLOY_SSH_KEY }}"'
    '(.jobs."deploy-demo".steps[] | select(.name == "Install the demo deploy key") | .env.DEMO_DEPLOY_PORT) = "${{ vars.WRONG_PORT }}"'
    '(.jobs."deploy-demo".steps[] | select(.name == "Install the demo deploy key") | .run) |= sub(">= 1"; ">= 0")'
    '(.jobs."deploy-demo".steps[] | select(.name == "Install the demo deploy key") | .run) |= sub("<= 65535"; "<= 65536")'
    '(.jobs."deploy-demo".steps[] | select(.run // "" | test("(^|\\s)ssh\\s")) | .run) |= sub("StrictHostKeyChecking=yes"; "StrictHostKeyChecking=no")'
    '(.jobs."deploy-demo".steps[] | select(.run // "" | test("(^|\\s)ssh\\s")) | .run) |= sub("ssh -T -p"; "ssh -T")'
    '(.jobs."deploy-demo".steps[] | select(.run // "" | test("(^|\\s)ssh\\s")) | .env.DEPLOY_SHA) = "${{ github.sha }}"'
  )
  for mutation in "${demo_mutations[@]}"; do
    yq "$mutation" "$workflow" > "$tmp/deploy.yml"
    if DEPLOY_WORKFLOW="$tmp/deploy.yml" "$0" --contract-only 2>/dev/null; then
      printf 'contract accepted a broken demo workflow: %s\n' "$mutation" >&2
      exit 1
    fi
  done

  local ci_mutations=(
    '.on.push.branches = ["main", "release"]'
    '.concurrency.group = "CI-run"'
    '.concurrency."cancel-in-progress" = false'
  )
  for mutation in "${ci_mutations[@]}"; do
    yq "$mutation" "$ci_workflow" > "$tmp/ci.yml"
    if CI_WORKFLOW="$tmp/ci.yml" "$0" --contract-only 2>/dev/null; then
      printf 'contract accepted a broken CI workflow: %s\n' "$mutation" >&2
      exit 1
    fi
  done
}

check_contract
check_ci_contract
if [[ "${1:-}" != '--contract-only' ]]; then
  self_test
  printf 'deploy workflow contract holds\n'
fi
