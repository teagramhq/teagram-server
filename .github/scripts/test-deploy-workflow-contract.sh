#!/usr/bin/env bash
# Guards the production deploy workflow: it may run only after CI passes on a
# push to main (or by manual dispatch), one at a time, in the production
# environment. It deploys the commit whose CI run passed; the canonical host
# script runs the MTProto smoke from that deployed image after readiness.
# shellcheck disable=SC2016 # single-quoted strings are literal workflow text
set -euo pipefail

workflow="${DEPLOY_WORKFLOW:-.github/workflows/deploy.yml}"
demo_workflow="${DEMO_DEPLOY_WORKFLOW:-.github/workflows/deploy-demo.yml}"
ci_workflow="${CI_WORKFLOW:-.github/workflows/ci.yml}"
dockerfile="${DEPLOY_DOCKERFILE:-Dockerfile}"
demo_forced_command="deploy/teagram-demo/forced-command.sh"
expected_if="\${{ github.event_name == 'workflow_dispatch' || (github.event.workflow_run.conclusion == 'success' && github.event.workflow_run.event == 'push') }}"
expected_demo_resolver_ref='${{ github.workflow_sha }}'
expected_demo_if="\${{ github.event.workflow_run.conclusion == 'success' && needs.check-server-deploy-result.outputs.succeeded == 'true' }}"
expected_demo_step_if="\${{ steps.demo-gate.outputs.enabled == 'true' }}"
expected_demo_cleanup_if="\${{ always() && steps.demo-gate.outputs.enabled == 'true' }}"
expected_demo_web_step_if="\${{ steps.web-gate.outputs.enabled == 'true' }}"
expected_demo_web_failure_if="\${{ steps.demo-gate.outputs.enabled == 'true' && steps.web-gate.outputs.enabled != 'true' }}"
expected_ci_group="\${{ github.workflow }}-\${{ github.event_name == 'pull_request' && format('pr-{0}', github.event.pull_request.number) || (github.event_name == 'push' && github.ref == 'refs/heads/main' && 'main') || format('run-{0}-{1}', github.run_id, github.run_attempt) }}"
expected_ci_cancel="\${{ github.event_name == 'pull_request' }}"
expected_run_name="\${{ inputs.issue && format('{0} [{1}]', github.workflow, inputs.issue) || '' }}"
expected_issue_description='Issue key to report this run to (e.g. MAIN-123). Leave empty for no report.'

fail() {
  printf '%s: %s\n' "$workflow" "$1" >&2
  exit 1
}

query() {
  if [[ "$1" == *'.jobs.deploy-demo'* ]]; then
    yq -r "$1" "$demo_workflow"
    return
  fi
  yq -r "$1" "$workflow"
}

demo_query() {
  yq -r "$1" "$demo_workflow"
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
  [[ "$(ci_query '.jobs."publish-demo-image".if')" == "\${{ always() && (needs.ops-prioritize-main.result == 'success' || (needs.ops-prioritize-main.result == 'skipped' && github.event_name == 'pull_request')) && success() && github.event_name == 'push' && github.ref == 'refs/heads/main' }}" ]] ||
    ci_fail 'the demo image may publish only from main pushes'
  [[ "$(ci_query '.jobs."publish-demo-image".needs | sort | join(",")')" == 'build-browser-acceptance-image,build-server-image,check-demo-web-pin,check-deploy-workflow,check-sqlc-drift,check-sticker-license,ci,lint-css,lint-go,lint-sql,mirror-rustfs-qualification-images,ops-prioritize-main,restore-readiness,rustfs-transition-qualification,test-browser-acceptance,test-compose,test-e2e-admin,test-go,test-migrations,test-photo-worker,test-real-server-fixtures,test-smoke,test-websocket-d1,test-websocket-permanent-exchange' ]] ||
    ci_fail 'the demo image publish must wait for every CI lane'
  [[ "$(ci_query '.jobs.ci.needs | join(",")')" == *'check-demo-web-pin'* ]] ||
    ci_fail 'the required CI gate must include the pinned demo web artifact build'
  [[ "$(ci_query '.jobs.ci.needs | join(",")')" == *'test-migrations'* ]] ||
    ci_fail 'the required CI gate must include the migrations lane'
  [[ "$(ci_query '.jobs."schema-gate-postgres" // ""')" == '' ]] ||
    ci_fail 'the obsolete PostgreSQL schema gate must be removed'
  [[ "$(ci_query '.jobs.ci.needs | join(",")')" != *'schema-gate-postgres'* ]] ||
    ci_fail 'the required CI gate must not include the obsolete PostgreSQL schema gate'
  local expected_postgres_image='mirror.gcr.io/library/postgres:18-alpine@sha256:d8703cd7fba306b9fec9268ecedfa8a966846c053036a60e3635791957eb2f66'
  [[ "$(ci_query '.jobs."test-migrations".env.POSTGRES_IMAGE')" == "$expected_postgres_image" ]] ||
    ci_fail 'the migrations lane must use the production-pinned PostgreSQL image'
  [[ "$(yq -r '.services.postgres.image' deploy/teagram-demo/compose.yaml)" == "$expected_postgres_image" ]] ||
    ci_fail 'the production PostgreSQL image pin changed without updating the migrations lane'
  [[ "$(ci_query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .env.CHECK_DEMO_WEB_PIN_RESULT')" == '${{ needs.check-demo-web-pin.result }}' ]] ||
    ci_fail 'the required CI gate must receive the pinned demo web artifact result'
  [[ "$(ci_query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .env.TEST_MIGRATIONS_RESULT')" == '${{ needs.test-migrations.result }}' ]] ||
    ci_fail 'the required CI gate must receive the migrations result'
  ci_query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run' |
    grep -Fq '"$CHECK_DEMO_WEB_PIN_RESULT" != "success"' ||
    ci_fail 'the required CI gate must reject a failed demo web artifact build'
  ci_query '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass") | .run' |
    grep -Fq '"$TEST_MIGRATIONS_RESULT" != "success"' ||
    ci_fail 'the required CI gate must reject a failed migrations lane'
  [[ "$(ci_query '.jobs."check-demo-web-pin".permissions.contents')" == 'read' ]] ||
    ci_fail 'the demo web artifact build must use read-only repository access'
  [[ "$(ci_query '.jobs."check-demo-web-pin".permissions | to_entries | map(.key + "=" + .value) | join(",")')" == 'contents=read' ]] ||
    ci_fail 'the demo web artifact build must not have action or package write access'
  [[ "$(ci_query '.jobs."check-demo-web-pin".environment // ""')" == '' ]] ||
    ci_fail 'the demo web artifact build must not use an environment with secrets'
  if ci_query '.jobs."check-demo-web-pin" | to_json' | grep -Fq 'secrets.'; then
    ci_fail 'the demo web artifact build must not reference secrets'
  fi
  [[ "$(ci_query '.jobs."check-demo-web-pin".steps[] | select(.name == "Rebuild and verify the pinned web artifact") | .env.WEB_SOURCE_SHA')" == '${{ steps.pin.outputs.source_commit }}' ]] ||
    ci_fail 'the demo web build must use the commit resolved from its pin'
  ci_query '.jobs."check-demo-web-pin".steps[] | select(.name == "Package the verified demo web artifact") | .run' |
    grep -Fq 'tar -czf "$archive" -C teagram-web/dist-private .' ||
    ci_fail 'the secret-free CI job must package the verified web build'
  ci_query '.jobs."check-demo-web-pin".steps[] | select(.name == "Package the verified demo web artifact") | .run' |
    grep -Fq 'archive.sha256' ||
    ci_fail 'the secret-free CI job must package the archive checksum with the web artifact'
  [[ "$(ci_query '.jobs."check-demo-web-pin".steps[] | select(.name == "Upload the verified demo web artifact") | .uses')" == 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' ]] ||
    ci_fail 'the verified web artifact must use the pinned upload action'
  [[ "$(ci_query '.jobs."check-demo-web-pin".steps[] | select(.name == "Upload the verified demo web artifact") | .with.name')" == 'demo-web-release-${{ steps.pin.outputs.source_commit }}' ]] ||
    ci_fail 'the verified web artifact name must include its pinned source commit'
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
  local runner_temp="$tmp/runner"
  mkdir -p "$runner_temp"
  # shellcheck disable=SC2030,SC2031
  export RUNNER_TEMP="$runner_temp"
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
    [[ "$(cat "$runner_temp/demo-deploy-target.json")" == '{"deploy_sha":"'"$expected"'","ci_run_id":"'"$expected_run_id"'"}' ]] ||
      fail "workflow_run must write the selected SHA and successful CI run to the demo handoff ($status)"
  done
  rm -f "$runner_temp/demo-deploy-target.json"
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
  [[ "$(cat "$runner_temp/demo-deploy-target.json")" == '{"deploy_sha":"'"$TEST_LATEST_SHA"'","ci_run_id":"'"$TEST_LATEST_RUN_ID"'"}' ]] ||
    fail 'manual dispatch must write the selected SHA and successful CI run to the demo handoff'
  TEST_LATEST_SHA=null
  rm -f "$runner_temp/demo-deploy-target.json"
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
  # shellcheck disable=SC2030,SC2031
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

check_demo_deploy_target_download() (
  set -euo pipefail
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  demo_query '.jobs.deploy-demo.steps[] | select(.id == "target") | .run' > "$tmp/download.sh"
  cat > "$tmp/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == run && "$2" == download && "$3" == "$TEST_DEPLOY_RUN_ID" ]]
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
[[ "$name" == "demo-deploy-target-$TEST_DEPLOY_RUN_ID" ]]
[[ "$destination" == "$RUNNER_TEMP/demo-deploy-target" ]]
mkdir -p "$destination"
case "$TEST_TARGET_CASE" in
  valid)
    printf '{"deploy_sha":"%s","ci_run_id":"%s"}\n' "$TEST_DEPLOY_SHA" "$TEST_CI_RUN_ID" > "$destination/demo-deploy-target.json"
    ;;
  missing-file) ;;
  malformed-json)
    printf '{"deploy_sha":"%s","ci_run_id":' "$TEST_DEPLOY_SHA" > "$destination/demo-deploy-target.json"
    ;;
  missing-deploy-sha)
    printf '{"ci_run_id":"%s"}\n' "$TEST_CI_RUN_ID" > "$destination/demo-deploy-target.json"
    ;;
  missing-ci-run-id)
    printf '{"deploy_sha":"%s"}\n' "$TEST_DEPLOY_SHA" > "$destination/demo-deploy-target.json"
    ;;
  invalid-deploy-sha)
    printf '{"deploy_sha":"not-a-sha","ci_run_id":"%s"}\n' "$TEST_CI_RUN_ID" > "$destination/demo-deploy-target.json"
    ;;
  invalid-ci-run-id)
    printf '{"deploy_sha":"%s","ci_run_id":"not-a-run-id"}\n' "$TEST_DEPLOY_SHA" > "$destination/demo-deploy-target.json"
    ;;
  *) exit 1 ;;
esac
SH
  chmod +x "$tmp/gh"
  # These overrides are consumed by the extracted workflow child process.
  # shellcheck disable=SC2030,SC2031
  export PATH="$tmp:$PATH" GITHUB_REPOSITORY=test/server
  # shellcheck disable=SC2030,SC2031
  export RUNNER_TEMP="$tmp/runner" GITHUB_OUTPUT="$tmp/output"
  # shellcheck disable=SC2030,SC2031
  export DEPLOY_RUN_ID=111111111 TEST_DEPLOY_RUN_ID=111111111
  # shellcheck disable=SC2030,SC2031
  export TEST_DEPLOY_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  # shellcheck disable=SC2030,SC2031
  export TEST_CI_RUN_ID=222222222

  local target_case
  for target_case in valid missing-file malformed-json missing-deploy-sha missing-ci-run-id invalid-deploy-sha invalid-ci-run-id; do
    : > "$GITHUB_OUTPUT"
    rm -rf "$RUNNER_TEMP/demo-deploy-target"
    if [[ "$target_case" == valid ]]; then
      TEST_TARGET_CASE="$target_case" GH_TOKEN=test-token bash -euo pipefail "$tmp/download.sh" > /dev/null 2>&1
      [[ "$(cat "$GITHUB_OUTPUT")" == $'sha='"$TEST_DEPLOY_SHA"$'\nci_run_id='"$TEST_CI_RUN_ID" ]] ||
        fail 'the demo deploy must emit the server SHA and selected CI run ID from its target artifact'
    elif TEST_TARGET_CASE="$target_case" GH_TOKEN=test-token bash -euo pipefail "$tmp/download.sh" > /dev/null 2>&1; then
      fail "the demo deploy must reject invalid target artifact metadata ($target_case)"
    fi
  done
)

check_demo_deploy_result() (
  set -euo pipefail
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  demo_query '.jobs.check-server-deploy-result.steps[] | select(.id == "verify") | .run' > "$tmp/verify.sh"
  cat > "$tmp/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == api ]]
[[ "$2" == "repos/test/server/actions/runs/$TEST_DEPLOY_RUN_ID/jobs?per_page=100" ]]
case "$TEST_DEPLOY_CONCLUSION" in
  missing) printf '{"jobs":[]}\n' ;;
  ambiguous) printf '{"jobs":[{"name":"deploy: server (prod)","conclusion":"success"},{"name":"deploy: server (prod)","conclusion":"success"}]}\n' ;;
  *) printf '{"jobs":[{"name":"deploy: server (prod)","conclusion":"%s"}]}\n' "$TEST_DEPLOY_CONCLUSION" ;;
esac
SH
  chmod +x "$tmp/gh"
  # These overrides are consumed by the extracted workflow child process.
  # shellcheck disable=SC2030,SC2031
  export PATH="$tmp:$PATH" GITHUB_REPOSITORY=test/server
  # shellcheck disable=SC2030,SC2031
  export DEPLOY_RUN_ID=111111111 TEST_DEPLOY_RUN_ID=111111111
  # shellcheck disable=SC2030,SC2031
  export GITHUB_OUTPUT="$tmp/output"

  local conclusion expected
  for conclusion in success skipped failure missing ambiguous; do
    : > "$GITHUB_OUTPUT"
    expected=false
    if [[ "$conclusion" == success ]]; then
      expected=true
    fi
    TEST_DEPLOY_CONCLUSION="$conclusion" GH_TOKEN=test-token \
      bash -euo pipefail "$tmp/verify.sh" > /dev/null
    [[ "$(cat "$GITHUB_OUTPUT")" == "succeeded=$expected" ]] ||
      fail "the demo gate must accept only one successful server deploy job ($conclusion)"
  done
)

check_demo_credential_gate() (
  set -euo pipefail
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  query '.jobs.deploy-demo.steps[] | select(.id == "demo-gate") | .run' > "$tmp/gate.sh"
  # shellcheck disable=SC2031
  export GITHUB_OUTPUT="$tmp/output"

  run_gate() {
    local enabled="$1" client_id="$2" oauth_secret="$3" expected_state="$4" expected_notice="$5" gate_log
    : > "$GITHUB_OUTPUT"
    gate_log="$(
      TEAGRAM_DEMO_DEPLOY_ENABLED="$enabled" \
        TS_OAUTH_CLIENT_ID="$client_id" \
        TS_OAUTH_SECRET="$oauth_secret" \
        bash -euo pipefail "$tmp/gate.sh"
    )"
    [[ "$(cat "$GITHUB_OUTPUT")" == "enabled=$expected_state" ]] ||
      fail 'the demo credential gate produced the wrong enabled state'
    if [[ -n "$expected_notice" ]]; then
      grep -Fq -- "$expected_notice" <<< "$gate_log" ||
        fail 'the demo credential gate must report why deployment was skipped'
    elif [[ -n "$gate_log" ]]; then
      fail 'the demo credential gate must not emit a notice when deployment is enabled'
    fi
    [[ "$gate_log" != *'fixture-client-id'* && "$gate_log" != *'fixture-oauth-secret'* ]] ||
      fail 'the demo credential gate must not log credential values'
  }

  local missing_credentials_notice='::notice::Public demo deployment skipped: Tailscale OAuth credentials are missing'
  run_gate true '' fixture-oauth-secret false "$missing_credentials_notice"
  run_gate true fixture-client-id '' false "$missing_credentials_notice"
  run_gate true fixture-client-id fixture-oauth-secret true ''
  run_gate false '' '' false '::notice::Public demo deployment is disabled'
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

  [[ "$(query '.jobs | keys | sort | join(",")')" == 'deploy-server' ]] ||
    fail 'the production workflow must contain only its server deploy job'
  [[ "$(query '.jobs."deploy-server".name')" == 'deploy: server (prod)' ]] ||
    fail 'the production deploy job must use the deploy stage naming contract'
  [[ "$(query '.jobs."deploy-server".if')" == "$expected_if" ]] ||
    fail 'the dev job must run only on dispatch or a successful CI push run'
  [[ "$(query '.jobs."deploy-server".environment')" == 'production' ]] ||
    fail 'the dev job must use environment: production'
  [[ "$(query '.jobs."deploy-server".permissions // {} | to_entries | map(select(.value != "read")) | length')" == 0 ]] ||
    fail 'the dev job must not request write permissions'
  [[ "$(query '.jobs."deploy-server".permissions.actions')" == 'read' ]] ||
    fail 'the dev job must grant actions: read to resolve successful CI runs'
  [[ "$(query '.jobs.deploy-server.steps[] | select(.name == "Upload the demo deployment target") | .uses')" == 'actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a' ]] ||
    fail 'the production deploy must upload its selected SHA and CI run for the demo workflow'
  [[ "$(query '.jobs.deploy-server.steps[] | select(.name == "Upload the demo deployment target") | .with.name')" == 'demo-deploy-target-${{ github.run_id }}' ]] ||
    fail 'the demo target artifact must be named for its Deploy. run'
  [[ "$(query '.jobs.deploy-server.steps[] | select(.name == "Upload the demo deployment target") | .with."if-no-files-found"')" == 'error' ]] ||
    fail 'a missing demo target artifact must fail its upload step visibly'
  [[ "$(query '.jobs.deploy-server.steps[] | select(.name == "Upload the demo deployment target") | ."continue-on-error"')" == 'true' ]] ||
    fail 'a demo handoff upload error must not block the production deploy'
  local target_run
  target_run="$(query '.jobs.deploy-server.steps[] | select(.id == "target") | .run')"
  grep -Fq 'demo-deploy-target.json' <<< "$target_run" ||
    fail 'the production deploy must write the tested target for the demo workflow'
  grep -Fq '"deploy_sha":"%s","ci_run_id":"%s"' <<< "$target_run" ||
    fail 'the demo target must contain the selected server SHA and CI run ID'
  local handoff_step_index prod_ssh_step_index cleanup_step_index
  handoff_step_index="$(grep -nF -m1 '      - name: Upload the demo deployment target' "$workflow" | cut -d: -f1)"
  prod_ssh_step_index="$(grep -nF -m1 '      - name: Deploy the tested commit' "$workflow" | cut -d: -f1)"
  cleanup_step_index="$(grep -nF -m1 '      - name: Remove the deploy key' "$workflow" | cut -d: -f1)"
  if [[ ! "$handoff_step_index" =~ ^[0-9]+$ || ! "$prod_ssh_step_index" =~ ^[0-9]+$ ||
    ! "$cleanup_step_index" =~ ^[0-9]+$ ]] ||
    (( handoff_step_index <= prod_ssh_step_index || handoff_step_index <= cleanup_step_index )); then
    fail 'the demo handoff must run after production deployment and key cleanup'
  fi

  [[ "$(demo_query '.name')" == 'Deploy demo.' ]] ||
    fail 'the demo must have its own workflow status and router result'
  [[ "$(demo_query '.on | keys | join(",")')" == 'workflow_run' ]] ||
    fail 'the demo workflow must run only from workflow_run'
  [[ "$(demo_query '.on.workflow_run.workflows | join(",")')" == 'Deploy.' ]] ||
    fail 'the demo workflow must start from the server Deploy. workflow'
  [[ "$(demo_query '.on.workflow_run.types | join(",")')" == 'completed' ]] ||
    fail 'the demo workflow must observe completed Deploy. runs'
  [[ "$(demo_query '.on.workflow_run.branches | join(",")')" == 'main' ]] ||
    fail 'the demo workflow must be limited to Deploy. runs from main'
  [[ "$(demo_query '.permissions | to_entries | map(.key + "=" + .value) | join(",")')" == 'contents=read' ]] ||
    fail 'the demo workflow must use contents: read only'
  [[ "$(demo_query '.concurrency.group')" == 'deploy-teagram-demo' ]] ||
    fail 'demo deploys must share their own concurrency group'
  [[ "$(demo_query '.concurrency."cancel-in-progress"')" == 'false' ]] ||
    fail 'demo deploys must not cancel an in-progress rollout'
  [[ "$(demo_query '.jobs | keys | sort | join(",")')" == 'check-server-deploy-result,deploy-demo' ]] ||
    fail 'the demo workflow must verify the triggering deploy before deploying'
  [[ "$(demo_query '.jobs.check-server-deploy-result.name')" == 'check: server-deploy-result' ]] ||
    fail 'the server deploy verification job must follow the check-stage naming contract'
  [[ "$(demo_query '.jobs.check-server-deploy-result.permissions | to_entries | map(.key + "=" + .value) | join(",")')" == 'actions=read' ]] ||
    fail 'the verification job must use actions: read only'
  [[ "$(demo_query '.jobs.check-server-deploy-result.outputs.succeeded')" == '${{ steps.verify.outputs.succeeded }}' ]] ||
    fail 'the verified deploy result must gate the demo job'
  [[ "$(demo_query '.jobs.check-server-deploy-result.steps[] | select(.id == "verify") | .env.DEPLOY_RUN_ID')" == '${{ github.event.workflow_run.id }}' ]] ||
    fail 'the verifier must inspect the workflow run that triggered the demo'
  local deploy_verify_run
  deploy_verify_run="$(demo_query '.jobs.check-server-deploy-result.steps[] | select(.id == "verify") | .run')"
  grep -Fq 'actions/runs/$DEPLOY_RUN_ID/jobs?per_page=100' <<< "$deploy_verify_run" ||
    fail 'the verifier must inspect jobs from the triggering Deploy. run'
  grep -Fq 'select(.name == "deploy: server (prod)")' <<< "$deploy_verify_run" ||
    fail 'the verifier must identify the production server deploy job'
  [[ "$(demo_query '.jobs.deploy-demo.needs | join(",")')" == 'check-server-deploy-result' ]] ||
    fail 'the demo deployment must wait for server deploy verification'
  [[ "$(demo_query '.jobs.deploy-demo.if')" == "$expected_demo_if" ]] ||
    fail 'the demo job must require both workflow success and a successful server deploy job'
  [[ "$(demo_query '.jobs.deploy-demo.continue-on-error // ""')" == '' ]] ||
    fail 'the demo workflow must report deployment failures'
  [[ "$(demo_query '.jobs.deploy-demo.environment')" == 'demo' ]] ||
    fail 'the demo job must use the separate demo environment'
  [[ "$(query '.jobs.deploy-demo.steps[0].id')" == 'demo-gate' ]] ||
    fail 'the demo environment gate must be the first step in the demo environment'
  [[ "$(query '.jobs.deploy-demo.steps[0].env.TEAGRAM_DEMO_DEPLOY_ENABLED')" == '${{ vars.TEAGRAM_DEMO_DEPLOY_ENABLED }}' ]] ||
    fail 'the demo gate must read the environment-scoped deployment variable after environment setup'
  [[ "$(query '.jobs.deploy-demo.steps[0].env.TS_OAUTH_CLIENT_ID')" == '${{ secrets.TS_OAUTH_CLIENT_ID }}' ]] ||
    fail 'the demo gate must read the Tailscale OAuth client ID from the demo environment'
  [[ "$(query '.jobs.deploy-demo.steps[0].env.TS_OAUTH_SECRET')" == '${{ secrets.TS_OAUTH_SECRET }}' ]] ||
    fail 'the demo gate must read the Tailscale OAuth secret from the demo environment'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.id == "target") | .env.DEPLOY_RUN_ID')" == '${{ github.event.workflow_run.id }}' ]] ||
    fail 'the target artifact must be selected by the triggering Deploy. run ID'
  local demo_gate_run
  demo_gate_run="$(query '.jobs.deploy-demo.steps[0].run')"
  [[ "$demo_gate_run" == *'enabled=true'* && "$demo_gate_run" == *'enabled=false'* ]] ||
    fail 'the demo gate must explicitly set its output for both enabled and disabled states'
  grep -Fq '[[ -z "$TS_OAUTH_CLIENT_ID" || -z "$TS_OAUTH_SECRET" ]]' <<< "$demo_gate_run" ||
    fail 'the demo gate must skip when either Tailscale OAuth credential is missing'
  grep -Fq '::notice::Public demo deployment skipped: Tailscale OAuth credentials are missing' <<< "$demo_gate_run" ||
    fail 'the demo gate must report when it skips for missing Tailscale OAuth credentials'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.id == "demo-image") | .if')" == "$expected_demo_step_if" ]] ||
    fail 'the demo image download must run only when public demo deployment is enabled'
  [[ "$(query '[.jobs.deploy-demo.steps[] | select(has("continue-on-error")) | .id] | join(",")')" == 'pin' ]] ||
    fail 'only the web pin resolver may continue so the server deployment can finish'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.id == "pin") | ."continue-on-error"')" == 'true' ]] ||
    fail 'the web resolver outcome must be retained for the post-deploy failure'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.id == "web-gate") | .env.DEMO_WEB_DEPLOYABLE')" == '${{ steps.pin.outputs.deployable }}' ]] ||
    fail 'the web gate must evaluate the resolver deployability output'
  local demo_web_gate_run
  demo_web_gate_run="$(query '.jobs.deploy-demo.steps[] | select(.id == "web-gate") | .run')"
  grep -Fq 'DEMO_WEB_DEPLOYABLE' <<< "$demo_web_gate_run" ||
    fail 'the web gate must reject a non-deployable web pin'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Download the web artifact from tested CI") | .if')" == "$expected_demo_web_step_if" ]] ||
    fail 'web artifact download must run only when the published pin is deployable'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Join the tailnet") | .if')" == "$expected_demo_step_if" ]] ||
    fail 'the demo tailnet connection must run only when public demo deployment is enabled'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Install the demo deploy key") | .if')" == "$expected_demo_step_if" ]] ||
    fail 'the demo key installation must run only when public demo deployment is enabled'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Deploy the tested server commit after the dev deploy") | .if')" == "$expected_demo_step_if" ]] ||
    fail 'the demo server deployment must run when public demo deployment is enabled, regardless of web pin state'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Fail if the pinned web publication is not deployable") | .if')" == "$expected_demo_web_failure_if" ]] ||
    fail 'a rejected web pin must fail visibly after the public server deployment'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Publish the pinned web artifact on the demo origin") | .if')" == "$expected_demo_web_step_if" ]] ||
    fail 'the web publish step must require a deployable published pin'
  local demo_server_step_index demo_resolver_failure_step_index demo_web_download_step_index demo_web_pin_failure_step_index
  demo_server_step_index="$(grep -nF -m1 '      - name: Deploy the tested server commit after the dev deploy' "$demo_workflow" | cut -d: -f1)"
  demo_resolver_failure_step_index="$(grep -nF -m1 '      - name: Fail after server deploy when web resolution failed' "$demo_workflow" | cut -d: -f1)"
  demo_web_download_step_index="$(grep -nF -m1 '      - name: Download the web artifact from tested CI' "$demo_workflow" | cut -d: -f1)"
  demo_web_pin_failure_step_index="$(grep -nF -m1 '      - name: Fail if the pinned web publication is not deployable' "$demo_workflow" | cut -d: -f1)"
  if [[ ! "$demo_server_step_index" =~ ^[0-9]+$ || ! "$demo_web_download_step_index" =~ ^[0-9]+$ ||
    ! "$demo_resolver_failure_step_index" =~ ^[0-9]+$ || ! "$demo_web_pin_failure_step_index" =~ ^[0-9]+$ ]] ||
    (( demo_server_step_index >= demo_web_download_step_index ||
      demo_server_step_index >= demo_resolver_failure_step_index ||
      demo_resolver_failure_step_index >= demo_web_download_step_index ||
      demo_web_download_step_index >= demo_web_pin_failure_step_index )); then
    fail 'web pin or artifact failures must not prevent the public server deployment from running'
  fi
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Deploy the tested server commit after the dev deploy") | .id')" == 'server-deploy' ]] ||
    fail 'the server deployment must expose its result to the resolver failure gate'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Fail after server deploy when web resolution failed") | .if')" == "\${{ always() && steps.demo-gate.outputs.enabled == 'true' && steps.pin.outcome == 'failure' && steps.server-deploy.outcome == 'success' }}" ]] ||
    fail 'resolver errors must fail visibly only after a successful demo server deployment'
  local resolver_failure_run
  resolver_failure_run="$(query '.jobs.deploy-demo.steps[] | select(.name == "Fail after server deploy when web resolution failed") | .run')"
  grep -Fq '::error::Public server deployed, but pinned web artifact resolution failed' <<< "$resolver_failure_run" ||
    fail 'resolver errors after server deployment must be annotated'
  grep -Fq 'exit 1' <<< "$resolver_failure_run" ||
    fail 'resolver errors after server deployment must fail the demo workflow'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Remove the demo deploy key") | .if')" == "$expected_demo_cleanup_if" ]] ||
    fail 'the demo deploy key must be removed even when deployment fails'
  [[ "$(query '.jobs.deploy-demo.permissions // {} | to_entries | map(select(.value != "read")) | length')" == 0 ]] ||
    fail 'the demo job must not request write permissions'
  [[ "$(query '.jobs.deploy-demo.permissions.actions')" == 'read' ]] ||
    fail 'the demo job must use read-only access to the tested CI artifact'
  local web_download_run
  web_download_run="$(query '.jobs.deploy-demo.steps[] | select(.name == "Download the web artifact from tested CI") | .run')"
  grep -Fq 'gh run download "$CI_RUN_ID"' <<< "$web_download_run" ||
    fail 'the credentialed demo job must download the web artifact from the selected successful CI run'
  grep -Fq 'demo-web-release-$WEB_SOURCE_SHA' <<< "$web_download_run" ||
    fail 'the web artifact download must be selected by the resolved pin source commit'
  grep -Fq 'sha256sum "$archive"' <<< "$web_download_run" ||
    fail 'the credentialed demo job must verify the archived web artifact checksum'

  [[ "$(query '.jobs[].steps[] | select(.id == "target") | .env.RUN_HEAD_SHA')" == '${{ github.event.workflow_run.head_sha }}' ]] ||
    fail 'the target step must take the CI run head_sha'
  [[ "$(query '.jobs[].steps[] | select(.id == "target") | .env.RUN_ID')" == '${{ github.event.workflow_run.id }}' ]] ||
    fail 'the target step must retain the triggering CI run id'
  query '.jobs[].steps[] | select(.id == "target") | .run' |
    grep -Fq 'actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=1' ||
    fail 'a manual run must resolve the newest successful CI push run on main'
  local ssh_steps demo_ssh_steps demo_port_run
  ssh_steps="$(query '[.jobs."deploy-server".steps[] | select(.run // "" | test("(^|\s)ssh\s"))] | length')"
  [[ "$ssh_steps" == 1 ]] || fail "expected one dev ssh step, found $ssh_steps"
  [[ "$(query '.jobs."deploy-server".steps[] | select(.run // "" | test("(^|\s)ssh\s")) | .env.DEPLOY_SHA')" == '${{ steps.target.outputs.sha }}' ]] ||
    fail 'the dev ssh step must take DEPLOY_SHA from the target step'
  query '.jobs."deploy-server".steps[] | select(.run // "" | test("(^|\s)ssh\s")) | .run' |
    grep -Fq 'root@100.124.236.66 "$DEPLOY_SHA"' ||
    fail 'the dev ssh step must send DEPLOY_SHA as its command'
  local checkout_steps setup_go_steps smoke_steps dev_step_names
  checkout_steps="$(query '[.jobs."deploy-server".steps[] | select(.uses // "" | test("^actions/checkout@"))] | length')"
  [[ "$checkout_steps" == 0 ]] || fail 'the server deploy runner must not check out tested source'
  checkout_steps="$(query '[.jobs.deploy-demo.steps[] | select(.uses // "" | test("^actions/checkout@"))] | length')"
  [[ "$checkout_steps" == 1 ]] || fail 'the credentialed demo job must check out only the trusted workflow source'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Check out the workflow source commit") | .with.ref')" == "$expected_demo_resolver_ref" ]] ||
    fail 'the credentialed demo job must check out the trusted workflow revision'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Check out the workflow source commit") | .with."persist-credentials"')" == 'false' ]] ||
    fail 'the workflow-source checkout must not persist its credential'
  [[ "$(query '[.jobs.deploy-demo.steps[] | select(.uses // "" | test("^pnpm/action-setup@|^actions/setup-node@"))] | length')" == 0 ]] ||
    fail 'the credentialed demo job must not install or run web build dependencies'
  setup_go_steps="$(query '[.jobs."deploy-server".steps[] | select(.uses // "" | test("^actions/setup-go@"))] | length')"
  [[ "$setup_go_steps" == 0 ]] || fail 'the deploy runner must not install Go for a host-side smoke'
  smoke_steps="$(query '[.jobs."deploy-server".steps[] | select(.name == "Run MTProto smoke on port 2443")] | length')"
  [[ "$smoke_steps" == 0 ]] || fail 'the deploy runner must not run the MTProto smoke'
  grep -Fq 'CGO_ENABLED=0 go build -o /mtproto-smoke ./cmd/mtproto-smoke' "$dockerfile" ||
    fail 'the deployed image must build the host-side MTProto smoke binary'
  grep -Fq 'COPY --from=build /mtproto-smoke /usr/local/bin/mtproto-smoke' "$dockerfile" ||
    fail 'the deployed image must contain the host-side MTProto smoke binary'
  dev_step_names="$(query '.jobs."deploy-server".steps | map(.name) | join("|")')"
  [[ "$dev_step_names" == *'Deploy the tested commit|Remove the deploy key|Upload the demo deployment target' && "$(query '.jobs."deploy-server".steps[-2].name')" == 'Remove the deploy key' ]] ||
    fail 'the deploy key must be removed after the host-side smoke and before demo handoff'
  [[ "$(query '.jobs."deploy-server".steps[] | select(.name == "Remove the deploy key") | .if')" == '${{ always() }}' ]] ||
    fail 'the deploy key must be removed even when deployment fails'
  demo_ssh_steps="$(query '[.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\s)ssh\s"))] | length')"
  [[ "$demo_ssh_steps" == 2 ]] || fail "expected server and web demo ssh steps, found $demo_ssh_steps"
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Deploy the tested server commit after the dev deploy") | .env.DEPLOY_SHA')" == '${{ steps.target.outputs.sha }}' ]] ||
    fail 'the demo ssh step must deploy the exact SHA from the Deploy. run'
  query '.jobs.deploy-demo.steps[] | select(.name == "Deploy the tested server commit after the dev deploy") | .run' |
    grep -Fq '"root@$DEMO_DEPLOY_HOST" "$DEPLOY_SHA $DEPLOY_IMAGE_DIGEST"' ||
    fail 'the demo ssh step must send the tested SHA and trusted image digest to the separate demo host'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Deploy the tested server commit after the dev deploy") | .env.DEPLOY_IMAGE_DIGEST')" == '${{ steps.demo-image.outputs.digest }}' ]] ||
    fail 'the demo ssh step must take its image digest from the trusted CI artifact'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.id == "demo-image") | .env.CI_RUN_ID')" == '${{ steps.target.outputs.ci_run_id }}' ]] ||
    fail 'the demo digest must be downloaded from the selected CI run'
  local digest_run
  digest_run="$(query '.jobs.deploy-demo.steps[] | select(.id == "demo-image") | .run')"
  grep -Fq 'gh run download "$CI_RUN_ID"' <<< "$digest_run" ||
    fail 'the demo image digest must be read from the trusted CI run artifact'
  grep -Fq 'demo-image-digest-$DEPLOY_SHA' <<< "$digest_run" ||
    fail 'the demo digest artifact must be selected by the exact tested SHA'
  grep -Fq '^sha256:[0-9a-f]{64}$' <<< "$digest_run" ||
    fail 'the downloaded image digest must be validated before SSH'
  if ! grep -Fq 'resolve_web_artifact_pin.py' "$demo_forced_command" ||
    ! grep -Fq -- '--verify-release-identity "$WEB_SOURCE_SHA" "$WEB_ARTIFACT_DIGEST"' "$demo_forced_command"; then
    fail 'the forced demo command must require the exact published web pin before accepting an archive'
  fi
  local pin_resolver_run
  pin_resolver_run="$(query '.jobs.deploy-demo.steps[] | select(.name == "Resolve the pinned web artifact") | .run')"
  grep -Fq 'resolve_web_artifact_pin.py --require-live-manifest' <<< "$pin_resolver_run" ||
    fail 'the deploy preflight must verify the published pin against the live manifest'
  query '.jobs.deploy-demo.steps[] | select(.name == "Publish the pinned web artifact on the demo origin") | .run' |
    grep -Fq '"web $DEPLOY_SHA $WEB_SOURCE_SHA $WEB_ARTIFACT_DIGEST $WEB_ARCHIVE_SHA256" < "$WEB_ARCHIVE"' ||
    fail 'the verified web archive must be streamed to the forced demo command'
  [[ "$(query '.jobs.deploy-demo.steps[] | select(.name == "Install the demo deploy key") | .env.DEMO_DEPLOY_PORT')" == '${{ vars.DEMO_DEPLOY_PORT }}' ]] ||
    fail 'the demo key step must validate the configured SSH port'
  [[ "$(query '[.jobs.deploy-demo.steps[] | select(.run // "" | test("(^|\\s)ssh\\s")) | .env.DEMO_DEPLOY_PORT] | unique | .[0]')" == '${{ vars.DEMO_DEPLOY_PORT }}' ]] ||
    fail 'both demo SSH steps must use the configured SSH port'
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
  [[ "$(demo_query '[.jobs[].steps[].run // "" | select(test("\$\{\{"))] | length')" == 0 ]] ||
    fail 'demo run blocks must not interpolate ${{ }}; pass values through env'

  local uses
  while IFS= read -r uses; do
    [[ -n "$uses" ]] || continue
    [[ "$uses" =~ ^[^@]+@[0-9a-f]{40}$ ]] || fail "action not pinned to a commit SHA: $uses"
  done < <(query '.jobs[].steps[].uses // ""')
  while IFS= read -r uses; do
    [[ -n "$uses" ]] || continue
    [[ "$uses" =~ ^[^@]+@[0-9a-f]{40}$ ]] || fail "action not pinned to a commit SHA: $uses"
  done < <(demo_query '.jobs[].steps[].uses // ""')
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
    '.jobs."deploy-server".if = "${{ github.event.workflow_run.event == '"'push'"' }}"'
    '.jobs."deploy-server".if = "${{ github.event.workflow_run.conclusion == '"'success'"' }}"'
    'del(.jobs."deploy-server".if)'
    'del(.jobs."deploy-server".environment)'
    'del(.concurrency)'
    '.concurrency.group = "deploy"'
    '.concurrency."cancel-in-progress" = true'
    '.jobs."deploy-server".steps += [{"name":"Checkout tested source","uses":"actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"}]'
    '.jobs."deploy-server".steps += [{"name":"Set up Go","uses":"actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e"}]'
    '.jobs."deploy-server".steps += [{"name":"Run MTProto smoke on port 2443","run":"go run ./cmd/mtproto-smoke"}]'
    '(.jobs."deploy-server".steps[] | select(.uses) | .uses) = "tailscale/github-action@v4"'
    '.permissions.contents = "write"'
    'del(.jobs."deploy-server".permissions.actions)'
    '(.jobs."deploy-server".steps[] | select(.id == "target") | .run) |= sub("== workflow_run"; "== disabled")'
    '(.jobs."deploy-server".steps[] | select(.id == "target") | .run) |= sub("comparison\" == ahead"; "comparison\" == behind")'
    '(.jobs."deploy-server".steps[] | select(.id == "target") | .run) |= sub("compare/\$sha...\$latest"; "compare/$latest...$sha")'
    '(.jobs."deploy-server".steps[] | select(.id == "target") | .run) |= sub("comparison\" == identical"; "comparison\" == diverged")'
    '(.jobs."deploy-server".steps[] | select(.id == "target") | .env.RUN_HEAD_SHA) = "${{ github.sha }}"'
    '(.jobs."deploy-server".steps[] | select(.id == "target") | .run) |= sub("event=push&"; "")'
    '(.jobs."deploy-server".steps[] | select(.env.DEPLOY_SHA) | .run) |= sub(" \"\$DEPLOY_SHA\""; "")'
    '(.jobs."deploy-server".steps[] | select(.env.DEPLOY_SHA) | .env.DEPLOY_SHA) = "${{ github.sha }}"'
    '(.jobs."deploy-server".steps[] | select(.env.DEPLOY_SHA) | .run) |= sub("\"\$DEPLOY_SHA\""; "${{ steps.target.outputs.sha }}")'
    '(.jobs."deploy-server".steps[] | select(.id == "target") | .run) |= sub("deploying \$sha"; "deploying ${{ github.sha }}")'
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
    '.jobs."deploy-demo".if = "${{ always() }}"'
    '.jobs."deploy-demo".continue-on-error = true'
    'del(.jobs."check-server-deploy-result")'
    '(.jobs."check-server-deploy-result".steps[] | select(.id == "verify") | .run) |= sub("deploy: server"; "wrong deploy job")'
    '.jobs."deploy-demo".steps |= map(select(.name != "Fail after server deploy when web resolution failed"))'
    '(.jobs."deploy-demo".steps[] | select(.id == "web-gate") | .env.DEMO_WEB_DEPLOYABLE) = "true"'
    '.jobs."deploy-demo".steps[0].run = "echo enabled=true"'
    '(.jobs."deploy-demo".steps[] | select(.name == "Publish the pinned web artifact on the demo origin") | .if) = "${{ always() }}"'
    '(.jobs."deploy-demo".steps[] | select(.name == "Deploy the tested server commit after the dev deploy") | .if) = "${{ always() }}"'
    '.jobs."deploy-demo".if = "${{ github.event_name == '"'"'workflow_dispatch'"'"' }}"'
    '.jobs."deploy-demo".steps[0].env.TEAGRAM_DEMO_DEPLOY_ENABLED = "true"'
    'del(.jobs."deploy-demo".steps[] | select(.id == "web-gate").env.DEMO_WEB_DEPLOYABLE)'
    'del(.jobs."deploy-demo".steps[0].env.TS_OAUTH_CLIENT_ID)'
    'del(.jobs."deploy-demo".steps[0].env.TS_OAUTH_SECRET)'
    '(.jobs."deploy-demo".steps[0].run) |= sub("TS_OAUTH_CLIENT_ID"; "WRONG_OAUTH_CLIENT_ID")'
    '(.jobs."deploy-demo".steps[0].run) |= sub("Tailscale OAuth credentials are missing"; "credentials missing")'
    '(.jobs."deploy-demo".steps[] | select(.id == "web-gate").run) |= sub("DEMO_WEB_DEPLOYABLE"; "WRONG_WEB_DEPLOYABLE")'
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
    yq "$mutation" "$demo_workflow" > "$tmp/deploy-demo.yml"
    if DEMO_DEPLOY_WORKFLOW="$tmp/deploy-demo.yml" "$0" --contract-only 2>/dev/null; then
      printf 'contract accepted a broken demo workflow: %s\n' "$mutation" >&2
      exit 1
    fi
  done

  local ci_mutations=(
    '.on.push.branches = ["main", "release"]'
    '.concurrency.group = "CI-run"'
    '.concurrency."cancel-in-progress" = false'
    '.jobs."publish-demo-image".needs = ["lint-go"]'
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
  check_demo_credential_gate
  check_demo_deploy_result
  check_demo_deploy_target_download
  self_test
  printf 'deploy workflow contract holds\n'
fi
