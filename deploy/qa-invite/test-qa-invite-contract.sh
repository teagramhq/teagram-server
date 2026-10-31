#!/usr/bin/env bash
# shellcheck disable=SC2016 # Literal snippets intentionally match workflow source.
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
COMPOSE=$SCRIPT_DIR/compose.yaml
WORKFLOW=$REPO_ROOT/.github/workflows/deploy-qa-invite.yml
FORCED_COMMAND=$REPO_ROOT/deploy/teagram-demo/forced-command.sh
WRITER=$SCRIPT_DIR/qa-account-writer.sh
REMOTE=$SCRIPT_DIR/remote.sh
INSTALLER=$REPO_ROOT/deploy/teagram-demo/ops/install-qa-invite-command.sh
ROLLBACK_TEST=$SCRIPT_DIR/test-qa-invite-rollback.sh
ROLLBACK=$SCRIPT_DIR/rollback.sh

fail() {
  printf 'qa-invite contract: %s\n' "$1" >&2
  exit 1
}

for path in "$COMPOSE" "$WORKFLOW" "$FORCED_COMMAND" "$WRITER" "$REMOTE" "$INSTALLER" "$ROLLBACK" "$ROLLBACK_TEST"; do
  [[ -f "$path" && ! -L "$path" ]] || fail "missing regular file: $path"
done

bash -n "$FORCED_COMMAND" "$WRITER" "$REMOTE" "$INSTALLER" "$ROLLBACK" "$ROLLBACK_TEST" || fail 'shell syntax failed'

[[ $(grep -c '^[[:space:]]*ports:' "$COMPOSE") -eq 1 ]] || fail 'Compose must publish exactly one service port'
grep -Fq 'TEAGRAM_QA_INVITE_BIND_ADDRESS=10.96.46.70' "$SCRIPT_DIR/.env.example" || fail 'Compose bind is not private to the demo LXC address'
grep -Fq 'name: teagram-qa-invite' "$COMPOSE" || fail 'Compose project name changed'
grep -Fq 'name: teagram-qa-invite-postgres' "$COMPOSE" || fail 'QA Postgres volume is not isolated'
grep -Fq 'name: teagram-qa-invite-identity' "$COMPOSE" || fail 'QA identity volume is not isolated'
grep -Fq 'name: teagram-qa-invite-blobs' "$COMPOSE" || fail 'QA blob volume is not isolated'
grep -Fq 'internal: true' "$COMPOSE" || fail 'QA backend network must be internal'
grep -Fq 'TG_REGISTRATION: invite' "$COMPOSE" || fail 'raw MTProto registration must use invite mode'
grep -Fq 'TG_USER_INVITE_QUOTA: "5"' "$COMPOSE" || fail 'invite quota must be five'
grep -Fq 'TG_USER_INVITE_MIN_ACCOUNT_AGE: 0' "$COMPOSE" || fail 'new operator accounts must be allowed to issue invites'
grep -Fq 'TG_PUBLIC_REGISTRATION: invite' "$COMPOSE" || fail 'WebSocket registration must use invite mode'
grep -Fq 'TG_TURNSTILE_SIGNIN_ENABLED: "false"' "$COMPOSE" || fail 'Turnstile must be disabled and reported absent'
grep -Fq 'TEAGRAM_QA_INVITE_IMAGE_REF' "$COMPOSE" || fail 'server image must be selected by immutable digest'
grep -Fq 'compose down --remove-orphans' "$REMOTE" || fail 'down must remove only QA project containers'
grep -Fq 'source "$REPO_ROOT/deploy/qa-invite/rollback.sh"' "$REMOTE" || fail 'up rollback helper is not loaded'
grep -Fq 'compose down --remove-orphans' "$ROLLBACK" || fail 'rollback must stop QA project containers'
! grep -Eq 'compose down .*--volumes|compose down .* -v' "$REMOTE" || fail 'down must preserve isolated state'
! grep -Fq 'compose down --remove-orphans || true' "$ROLLBACK" || fail 'up rollback must not ignore Compose shutdown errors'
bash "$ROLLBACK_TEST" || fail 'up rollback stop-before-restore fixtures failed'
grep -Fq '/home/qa/.qa-accounts-invite' "$WRITER" || fail 'operator credentials are not stored on the QA user'
grep -Fq "chmod 0600 \"\$temp\"" "$WRITER" || fail 'QA credential file mode is not restricted'
grep -Fq 'qa-invite\ ([0-9a-f]{40})\ (up)' "$FORCED_COMMAND" || fail 'forced command does not accept only the reviewed up operation'
grep -Fq 'create-account' "$FORCED_COMMAND" || fail 'forced command does not accept account creation'
grep -Fq "qa-invite \$SOURCE_SHA create-account \$ACCOUNT_NAME" "$WORKFLOW" || fail 'workflow account creation does not use the pinned main SHA'
grep -Fq 'QA_ACCOUNTS_SSH_KEY' "$WORKFLOW" || fail 'workflow has no restricted QA credential transfer path'
grep -Fq '[[ "$GITHUB_REF" == refs/heads/main ]]' "$WORKFLOW" || fail 'deployment workflow must reject non-main dispatches'
input_validation=$(sed -n '/- name: Validate operation and deployment settings/,/- name: Resolve the tested main image digest/p' "$WORKFLOW")
! grep -Fq '${{ secrets.' <<< "$input_validation" || fail 'deployment secrets must not be injected before main-ref validation'
grep -Fq 'Operator account result is unknown; its QA credential was retained for reconciliation' "$WORKFLOW" ||
  fail 'ambiguous account creation must retain the QA credential for reconciliation'
! grep -Fq '"remove ${ACCOUNT_NAME,,}"' "$WORKFLOW" || fail 'ambiguous account creation must not discard the saved credential'
grep -Fq '[[ ! -e "$path" && ! -L "$path" ]]' "$WORKFLOW" || fail 'workflow must verify temporary account credential cleanup'
grep -Fq 'Report operator account creation' "$WORKFLOW" || fail 'account success summary must run after temporary key cleanup'
credential_cleanup_line=$(grep -nF 'rm -f -- "$password_file" "$record_file" "$store_result_file" "$result_file" "$error_file"' "$WORKFLOW" | head -n 1 | cut -d: -f1)
credential_verify_line=$(grep -nF '[[ ! -e "$path" && ! -L "$path" ]]' "$WORKFLOW" | head -n 1 | cut -d: -f1)
account_output_line=$(grep -nF "printf 'account_name=%s" "$WORKFLOW" | head -n 1 | cut -d: -f1)
key_cleanup_line=$(grep -nF 'name: Remove deployment keys' "$WORKFLOW" | head -n 1 | cut -d: -f1)
success_report_line=$(grep -nF 'name: Report operator account creation' "$WORKFLOW" | head -n 1 | cut -d: -f1)
((credential_cleanup_line < credential_verify_line && credential_verify_line < account_output_line && key_cleanup_line < success_report_line)) ||
  fail 'account success must follow verified password and SSH key cleanup'
grep -Fq 'workflow_dispatch:' "$WORKFLOW" || fail 'deployment workflow must be manually dispatchable'
grep -Fq 'default: MAIN-3023' "$WORKFLOW" || fail 'workflow must carry its issue key input'
for operation in up down status create-account; do
  grep -Eq "^[[:space:]]+- $operation$" "$WORKFLOW" || fail "missing workflow operation: $operation"
done

printf '%s\n' 'qa-invite isolated deployment contract holds'
