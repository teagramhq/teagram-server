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
  [[ "$(ci_query '.jobs."publish-demo-image".if')" == "\${{ github.event_name == 'push' && github.ref == 'refs/heads/main' }}" ]] ||
    ci_fail 'the demo image may publish only from main pushes'
  [[ "$(ci_query '.jobs."publish-demo-image".needs | sort | join(",")')" == 'browser-acceptance,browser-acceptance-image,ci,ci-main,compose,css,deploy-workflow,docker,e2e-admin,lint,mirror-rustfs-qualification-images,photothumb-runtime,real-server-fixtures,restore-readiness,rustfs-transition-qualification,schema-gate-postgres,smoke,sql-lint,sqlc,websocket-d1,websocket-permanent-exchange' ]] ||
    ci_fail 'the demo image publish must wait for every CI lane'
  [[ "$(ci_query '.jobs."publish-demo-image".permissions | to_entries | sort_by(.key) | map(.key + "=" + .value) | join(",")')" == 'actions=write,contents=read,packages=write' ]] ||
    ci_fail 'the demo image publisher must use only the required package and cache permissions'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Build and publish the tested demo image") | .id')" == 'publish_image' ]] ||
    ci_fail 'the demo image build step must expose its digest output'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Log in to GHCR") | .env.GITHUB_TOKEN')" == '${{ secrets.GITHUB_TOKEN }}' ]] ||
    ci_fail 'the demo image publisher must authenticate with the workflow token'
  ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Log in to GHCR") | .run' |
    grep -Fq -- '--password-stdin' || ci_fail 'the demo publisher must send its token through stdin'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Build and publish the tested demo image") | .with.push')" == 'true' ]] ||
    ci_fail 'the demo image publisher must push its build output'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Build and publish the tested demo image") | .with.tags')" == 'ghcr.io/teagramhq/teagram-server-demo:sha-${{ github.sha }}' ]] ||
    ci_fail 'the demo image tag must contain the full tested SHA'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Build and publish the tested demo image") | .with."build-args"')" == *'TEAGRAMD_SOURCE_REVISION=${{ github.sha }}'* ]] ||
    ci_fail 'the demo image revision label must identify the tested SHA'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Build and publish the tested demo image") | .with.labels')" == *'org.opencontainers.image.source=https://github.com/teagramhq/teagram-server'* ]] ||
    ci_fail 'the published demo image must link to the public source repository'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Save tested demo image digest") | .env.IMAGE_DIGEST')" == '${{ steps.publish_image.outputs.digest }}' ]] ||
    ci_fail 'the digest artifact must come from the publisher output'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Upload tested demo image digest") | .uses')" == 'actions/upload-artifact@v7.0.1' ]] ||
    ci_fail 'the tested image digest must be attached to its CI run as an artifact'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Upload tested demo image digest") | .with.name')" == 'demo-image-digest-${{ github.sha }}' ]] ||
    ci_fail 'the tested image digest artifact must be named for its source SHA'
  [[ "$(ci_query '.jobs."publish-demo-image".steps[] | select(.name == "Upload tested demo image digest") | .with."if-no-files-found"')" == 'error' ]] ||
    ci_fail 'publishing must fail if the trusted image digest artifact is missing'
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
  'api repos/test/server/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=1 --jq .workflow_runs[0] | [.head_sha, (.id | tostring)] | @tsv')
    printf '%s\t%s\n' "$TEST_LATEST_SHA" "$TEST_LATEST_RUN_ID"
    ;;
  "api repos/test/server/compare/$RUN_HEAD_SHA...$TEST_LATEST_SHA --jq .status")
    [[ "$TEST_COMPARISON" != api-error ]] || exit 1
    printf '%s\n' "$TEST_COMPARISON"
    ;;
  *) echo "unexpected forge query: $*" >&2; exit 1 ;;
esac
SH
  chmod +x "$tmp/gh"
  # These overrides are consumed by the extracted workflow child process.
  # shellcheck disable=SC2030,SC2031
  export PATH="$tmp:$PATH" GITHUB_REPOSITORY=test/server
  export RUN_HEAD_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  export RUN_ID=111111111
  export TEST_LATEST_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  export TEST_LATEST_RUN_ID=222222222
  # shellcheck disable=SC2030,SC2031
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
    local expected_run_id="$RUN_ID"
    if [[ "$status" == ahead || "$status" == identical ]]; then
      expected_run_id="$TEST_LATEST_RUN_ID"
    fi
    [[ "$(cat "$GITHUB_OUTPUT")" == $'sha='"$expected"$'\nci_run_id='"$expected_run_id" ]] ||
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
  [[ "$(cat "$GITHUB_OUTPUT")" == $'sha='"$TEST_LATEST_SHA"$'\nci_run_id='"$TEST_LATEST_RUN_ID" ]] ||
    fail 'manual dispatch must select the latest successful CI push head'
  TEST_LATEST_SHA=null
  if bash -euo pipefail "$tmp/target.sh" > /dev/null; then
    fail 'an invalid successful CI head must fail the deploy'
  fi
)

check_demo_image_digest_download() (
  set -euo pipefail
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  query '.jobs.deploy-demo.steps[] | select(.id == "demo-image") | .run' > "$tmp/download.sh"
  cat > "$tmp/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == run && "$2" == download && "$3" == "$TEST_CI_RUN_ID" ]]
shift 3
repo=
name=
destination=
while (($#)); do
  case "$1" in
    --repo) repo="$2"; shift 2 ;;
    --name) name="$2"; shift 2 ;;
    --dir) destination="$2"; shift 2 ;;
    *) exit 1 ;;
  esac
done
[[ "$repo" == test/server ]]
[[ "$name" == "demo-image-digest-$TEST_DEPLOY_SHA" ]]
mkdir -p "$destination"
printf '%s\n' "$TEST_IMAGE_DIGEST" > "$destination/demo-image-digest.txt"
SH
  chmod +x "$tmp/gh"
  # These overrides are consumed by the extracted workflow child process.
  # shellcheck disable=SC2030,SC2031
  export PATH="$tmp:$PATH" GITHUB_REPOSITORY=test/server
  # shellcheck disable=SC2030,SC2031
  export RUNNER_TEMP="$tmp/runner" GITHUB_OUTPUT="$tmp/output"
  export CI_RUN_ID=222222222 DEPLOY_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  export TEST_CI_RUN_ID="$CI_RUN_ID" TEST_DEPLOY_SHA="$DEPLOY_SHA"
  export TEST_IMAGE_DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  GH_TOKEN=test-token bash -euo pipefail "$tmp/download.sh"
  [[ "$(cat "$GITHUB_OUTPUT")" == "digest=$TEST_IMAGE_DIGEST" ]] ||
    fail 'the demo deploy must pass through the digest from the selected CI run artifact'
  : > "$GITHUB_OUTPUT"
  TEST_IMAGE_DIGEST=untrusted-value
  export TEST_IMAGE_DIGEST
  if GH_TOKEN=test-token bash -euo pipefail "$tmp/download.sh"; then
    fail 'the demo deploy must reject an invalid digest from the CI artifact'
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
  [[ "$(query '.jobs.deploy.outputs.ci_run_id')" == '${{ steps.target.outputs.ci_run_id }}' ]] ||
    fail 'the dev job must expose the exact successful CI run to the demo job'
  [[ "$(query '.jobs.deploy-demo.needs')" == 'deploy' ]] ||
    fail 'the demo job must wait for a successful dev deployment'
  [[ "$(query '.jobs.deploy-demo.if')" == "$expected_demo_if" ]] ||
    fail 'the demo job must require both a successful dev job and the explicit feature gate'
  [[ "$(query '.jobs.deploy-demo.environment')" == 'demo' ]] ||
    fail 'the demo job must use the separate demo environment'
  [[ "$(query '.jobs.deploy-demo.permissions // {} | to_entries | map(select(.value != "read")) | length')" == 0 ]] ||
    fail 'the demo job must not request write permissions'
  [[ "$(query '.jobs.deploy-demo.permissions.actions')" == 'read' ]] ||
    fail 'the demo job must use read-only access to the tested CI artifact'

  [[ "$(query '.jobs[].steps[] | select(.id == "target") | .env.RUN_HEAD_SHA')" == '${{ github.event.workflow_run.head_sha }}' ]] ||
    fail 'the target step must take the CI run head_sha'
  [[ "$(query '.jobs[].steps[] | select(.id == "target") | .env.RUN_ID')" == '${{ github.event.workflow_run.id }}' ]] ||
    fail 'the target step must retain the triggering CI run id'
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
    grep -Fq '"root@$DEMO_DEPLOY_HOST" "$DEPLOY_SHA $DEPLOY_IMAGE_DIGEST"' ||
    fail 'the demo ssh step must send the tested SHA and trusted image digest to the separate demo host'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.run // "" | contains("ssh -T -p")) | .env.DEPLOY_IMAGE_DIGEST')" == '${{ steps.demo-image.outputs.digest }}' ]] ||
    fail 'the demo ssh step must take its image digest from the trusted CI artifact'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.id == "demo-image") | .env.CI_RUN_ID')" == '${{ needs.deploy.outputs.ci_run_id }}' ]] ||
    fail 'the demo digest must be downloaded from the selected CI run'
  local digest_run
  digest_run="$(query '.jobs.deploy-demo.steps[] | select(.id == "demo-image") | .run')"
  grep -Fq 'gh run download "$CI_RUN_ID"' <<< "$digest_run" ||
    fail 'the demo image digest must be read from the trusted CI run artifact'
  grep -Fq 'demo-image-digest-$DEPLOY_SHA' <<< "$digest_run" ||
    fail 'the demo digest artifact must be selected by the exact tested SHA'
  grep -Fq '^sha256:[0-9a-f]{64}$' <<< "$digest_run" ||
    fail 'the downloaded image digest must be validated before SSH'
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
  check_demo_image_digest_download
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
    '.jobs."publish-demo-image".needs = ["lint"]'
    '.jobs."publish-demo-image".permissions.packages = "read"'
    '(.jobs."publish-demo-image".steps[] | select(.name == "Build and publish the tested demo image") | .with.tags) = "ghcr.io/teagramhq/teagram-server-demo:latest"'
    '(.jobs."publish-demo-image".steps[] | select(.name == "Build and publish the tested demo image") | .with.push) = false'
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
