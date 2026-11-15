#!/usr/bin/env bash
# shellcheck disable=SC2016 # Match literal shell expressions in the workflow.
set -Eeuo pipefail

workflow=.github/workflows/qa-bot.yml
query() { yq -r "$1" "$workflow"; }
fail() { printf 'qa-bot workflow contract: %s\n' "$1" >&2; exit 1; }

[[ "$(query '.on | keys | join(",")')" == workflow_dispatch ]] || fail 'only manual dispatch is allowed'
[[ "$(query '.on.workflow_dispatch.inputs.action.options | join(",")')" == create,rotate,revoke,status,probe ]] || fail 'operation allowlist changed'
[[ "$(query '.on.workflow_dispatch.inputs | keys | sort | join(",")')" == action,issue ]] || fail 'unexpected dispatch input'
[[ "$(query '.jobs.ops-qa-bot.environment')" == k3s-prod ]] || fail 'cluster credentials must stay in k3s-prod'
[[ "$(query '.jobs.check-qa-bot-writer.environment')" == demo && "$(query '.jobs.ops-qa-bot-delivery.environment')" == demo ]] || fail 'QA writer credential must stay in demo'
[[ "$(query '.concurrency.group')" == "$(yq -r '.concurrency.group' .github/workflows/deploy.yml)" ]] || fail 'bot operations must hold the deploy lock through delivery'
[[ "$(query '.concurrency."cancel-in-progress"')" == false ]] || fail 'bot requests must not cancel an active deploy'
[[ "$(query '.concurrency.queue')" == max ]] || fail 'bot requests must not replace a pending deploy'
[[ "$(yq -r '.concurrency.queue' .github/workflows/deploy.yml)" == max ]] || fail 'deploy requests must not replace a pending deploy'
[[ "$(query '[.jobs | to_entries[] | select(.value.concurrency.queue != null) | .key] | join(",")')" == '' ]] || fail 'only the workflow may declare a queue'
validation=$(query '.jobs.check-qa-bot-request.steps[0].run')
[[ "$validation" == *'"$GITHUB_REF" == refs/heads/main'* && "$validation" == *'"$API_READY" == true'* ]] || fail 'main and readiness must be checked before credentials'
[[ "$(query '.jobs.ops-qa-bot.needs | join(",")')" == check-qa-bot-request,check-qa-bot-writer ]] || fail 'QA preflight must precede the cluster job'
cluster_steps=$(query '.jobs.ops-qa-bot.steps[] | .run // ""')
cluster_step_names=$(query '.jobs.ops-qa-bot.steps[].name')
[[ "$cluster_step_names" == *$'Check production deployment is idle\nTake and verify a pre-operation backup'* ]] || fail 'the deploy guard must precede the backup'
[[ "$cluster_steps" == *'kubectl create --dry-run=server'* && "$cluster_steps" == *'--from=cronjob/postgres-backup'* ]] || fail 'server dry-run and backup are required'
[[ "$cluster_steps" == *'gh attestation verify'* && "$cluster_steps" == *'docker buildx imagetools inspect'* ]] || fail 'live image provenance must be verified'
[[ "$cluster_steps" == *'bash deploy/k8s/wait-qa-bot-job.sh'* ]] || fail 'the job must use the sanitized failure detector'
delivery=$(query '.jobs.ops-qa-bot-delivery.steps[] | .run // ""')
[[ "$delivery" == *'bot-store'* && "$delivery" == *'bot-probe'* ]] || fail 'restricted delivery and live probe are required'
[[ "$delivery" != *'kubectl exec'* ]] || fail 'the QA writer must not obtain pod exec'
[[ "$(query '.jobs.ops-qa-bot-delivery.if')" == "\${{ needs.ops-qa-bot.result == 'success' && (needs.ops-qa-bot.outputs.generated == 'true' || inputs.action == 'probe') }}" ]] || fail 'delivery must run only after generation or an explicit probe'
[[ "$(query '.jobs.ops-qa-bot.outputs.artifact_name')" == '${{ steps.operation.outputs.artifact_name }}' ]] || fail 'the producer must expose its artifact name'
[[ "$(query '.jobs.ops-qa-bot.steps[] | select(.name == "Transfer only the encrypted token") | .with.name')" == '${{ steps.operation.outputs.artifact_name }}' ]] || fail 'upload must use the producer name'
[[ "$(query '.jobs.ops-qa-bot-delivery.steps[] | select(.name == "Download the encrypted token") | .with.name')" == '${{ needs.ops-qa-bot.outputs.artifact_name }}' ]] || fail 'delivery retry must use the original producer name'

tmp=$(mktemp -d "${TMPDIR:-/tmp}/qa-bot-contract.XXXXXXXX")
cleanup() { case "$tmp" in */qa-bot-contract.*) rm -rf -- "$tmp" ;; *) return 1 ;; esac; }
trap cleanup EXIT
query '.jobs.ops-qa-bot.steps[] | select(.name == "Take and verify a pre-operation backup") | .run' > "$tmp/backup.sh"
query '.jobs.ops-qa-bot.steps[] | select(.name == "Check production deployment is idle") | .run' > "$tmp/deploy-guard.sh"
cat > "$tmp/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  create|wait) ;;
  rollout)
    [[ "${2:-}" == status && "${3:-}" == statefulset/teagramd && " $* " == *' --watch=false '* ]]
    [[ "${MOCK_ROLLOUT:-stable}" == stable ]]
    ;;
  get) printf '%s\n' '{"status":{"conditions":[{"type":"Complete","status":"True"}]}}' ;;
  logs)
    if [[ -n "${MOCK_RESULT:-}" ]]; then cat "$MOCK_RESULT"; exit 0; fi
    if [[ "${MOCK_BACKUP:-}" != missing ]]; then
      printf '%s\n' 'backup=verified path=/backups/teagram-20261115T120000Z-ci.dump bytes=123 sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    fi
    if [[ "${MOCK_BACKUP:-}" == duplicate ]]; then
      printf '%s\n' 'backup=verified path=/backups/teagram-20261115T120001Z-ci.dump bytes=123 sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    fi
    printf '%s\n' 'backup=retention-complete retained=7'
    ;;
  *) exit 99 ;;
esac
MOCK
chmod 0700 "$tmp/kubectl"
cat > "$tmp/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ "${MOCK_GH_FAIL:-}" != 1 ]] || exit 1
[[ "${MOCK_GH_INVALID:-}" != 1 ]] || { printf '%s\n' 'not-json'; exit 0; }
[[ "${1:-}" == run && "${2:-}" == list ]]
status='' workflow='' repo=''
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --status) status=$2; shift 2 ;;
    --workflow) workflow=$2; shift 2 ;;
    --repo) repo=$2; shift 2 ;;
    *) shift ;;
  esac
done
[[ "$workflow" == deploy.yml && "$repo" == teagramhq/teagram-server ]]
if [[ "$status" == "${MOCK_DEPLOY_STATUS:-none}" ]]; then
  printf '[{"databaseId":17,"status":"%s"}]\n' "$status"
else
  printf '%s\n' '[]'
fi
MOCK
chmod 0700 "$tmp/gh"
export PATH="$tmp:$PATH" GITHUB_RUN_ID=123 GITHUB_RUN_ATTEMPT=1 GITHUB_REPOSITORY=teagramhq/teagram-server
bash "$tmp/deploy-guard.sh" > "$tmp/output" || fail 'an idle deploy and settled rollout were rejected'
for state in queued in_progress pending requested waiting; do
  if MOCK_DEPLOY_STATUS="$state" bash "$tmp/deploy-guard.sh" > "$tmp/output" 2>&1; then fail "active deploy state $state was accepted"; fi
done
if MOCK_GH_FAIL=1 bash "$tmp/deploy-guard.sh" > "$tmp/output" 2>&1; then fail 'deploy API failure was accepted'; fi
if MOCK_GH_INVALID=1 bash "$tmp/deploy-guard.sh" > "$tmp/output" 2>&1; then fail 'invalid deploy API response was accepted'; fi
if MOCK_ROLLOUT=unstable bash "$tmp/deploy-guard.sh" > "$tmp/output" 2>&1; then fail 'unsettled server rollout was accepted'; fi
bash "$tmp/backup.sh" > "$tmp/output" || fail 'normal backup and retention output was rejected'
grep -Fxq 'backup=verified path=/backups/teagram-20261115T120000Z-ci.dump bytes=123 sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$tmp/output" || fail 'verified backup path was not reported'
if MOCK_BACKUP=missing bash "$tmp/backup.sh" >/dev/null; then fail 'missing backup was accepted'; fi
if MOCK_BACKUP=duplicate bash "$tmp/backup.sh" >/dev/null; then fail 'ambiguous backup was accepted'; fi

query '.jobs.ops-qa-bot.steps[] | select(.name == "Run the pinned bot operation") | .run' > "$tmp/operation.sh"
export RUNNER_TEMP="$tmp" GITHUB_OUTPUT="$tmp/github-output" GITHUB_STEP_SUMMARY="$tmp/summary" IMAGE=ghcr.io/teagramhq/teagramd:ci
export MOCK_RESULT="$tmp/result" ACTION=create
printf '%s\n' 'action=create bot_id=123 username=teagram_qa_test_bot' \
  '-----BEGIN AGE ENCRYPTED FILE-----' 'ciphertext' '-----END AGE ENCRYPTED FILE-----' > "$MOCK_RESULT"
bash "$tmp/operation.sh" > "$tmp/output" || fail 'encrypted token result was rejected'
grep -Fxq 'generated=true' "$GITHUB_OUTPUT" || fail 'encrypted token was not selected for delivery'
grep -Fxq 'artifact_name=qa-bot-token-123-1' "$GITHUB_OUTPUT" || fail 'the first attempt did not publish its artifact name'
GITHUB_RUN_ATTEMPT=2
artifact_name=$(sed -n 's/^artifact_name=//p' "$GITHUB_OUTPUT")
[[ "$artifact_name" != "qa-bot-token-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT" ]] || fail 'retry substituted the delivery attempt for the producer attempt'
grep -Fxq -- '-----BEGIN AGE ENCRYPTED FILE-----' "$tmp/qa-bot-token.age" || fail 'encrypted token was not preserved'
ACTION=rotate
if bash "$tmp/operation.sh" > "$tmp/output" 2>&1; then fail 'result for a different action was accepted'; fi
ACTION=status
printf '%s\n' 'state=missing username=teagram_qa_test_bot' > "$MOCK_RESULT"
: > "$GITHUB_OUTPUT"
bash "$tmp/operation.sh" > "$tmp/output" || fail 'missing-bot status was rejected'
grep -Fxq 'generated=false' "$GITHUB_OUTPUT" || fail 'status unexpectedly selected a token for delivery'
printf '%s\n' '123:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA' > "$MOCK_RESULT"
if bash "$tmp/operation.sh" > "$tmp/output" 2>&1; then fail 'plaintext token was accepted as metadata'; fi
if grep -Fq '123:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA' "$tmp/output"; then fail 'plaintext token was printed on failure'; fi
ACTION=create
printf '%s\n' 'state=exists bot_id=123 username=teagram_qa_test_bot token_active=true' > "$MOCK_RESULT"
: > "$GITHUB_STEP_SUMMARY"
bash "$tmp/operation.sh" > "$tmp/output" || fail 'existing active bot was rejected'
grep -Fxq 'QA bot already exists; no token was issued or delivered.' "$GITHUB_STEP_SUMMARY" || fail 'existing bot must be reported without claiming delivery'
python3 -B deploy/k8s/test_qa_bot_concurrency.py
printf '%s\n' 'QA bot workflow contract holds'
