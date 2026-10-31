#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
COMPOSE=$SCRIPT_DIR/compose.yaml
WORKFLOW=$REPO_ROOT/.github/workflows/deploy-qa-invite.yml
FORCED_COMMAND=$REPO_ROOT/deploy/teagram-demo/forced-command.sh
WRITER=$SCRIPT_DIR/qa-account-writer.sh
REMOTE=$SCRIPT_DIR/remote.sh
INSTALLER=$REPO_ROOT/deploy/teagram-demo/ops/install-qa-invite-command.sh

fail() {
  printf 'qa-invite contract: %s\n' "$1" >&2
  exit 1
}

for path in "$COMPOSE" "$WORKFLOW" "$FORCED_COMMAND" "$WRITER" "$REMOTE" "$INSTALLER"; do
  [[ -f "$path" && ! -L "$path" ]] || fail "missing regular file: $path"
done

bash -n "$FORCED_COMMAND" "$WRITER" "$REMOTE" "$INSTALLER" || fail 'shell syntax failed'

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
! grep -Eq 'compose down .*--volumes|compose down .* -v' "$REMOTE" || fail 'down must preserve isolated state'
grep -Fq '/home/qa/.qa-accounts-invite' "$WRITER" || fail 'operator credentials are not stored on the QA user'
grep -Fq 'chmod 0600 "$temp"' "$WRITER" || fail 'QA credential file mode is not restricted'
grep -Fq 'qa-invite\ ([0-9a-f]{40})\ (up)' "$FORCED_COMMAND" || fail 'forced command does not accept only the reviewed up operation'
grep -Fq 'create-account' "$FORCED_COMMAND" || fail 'forced command does not accept account creation'
grep -Fq 'qa-invite $SOURCE_SHA create-account $ACCOUNT_NAME' "$WORKFLOW" || fail 'workflow account creation does not use the pinned main SHA'
grep -Fq 'QA_ACCOUNTS_SSH_KEY' "$WORKFLOW" || fail 'workflow has no restricted QA credential transfer path'
grep -Fq 'workflow_dispatch:' "$WORKFLOW" || fail 'deployment workflow must be manually dispatchable'
grep -Fq 'default: MAIN-3023' "$WORKFLOW" || fail 'workflow must carry its issue key input'
for operation in up down status create-account; do
  grep -Eq "^[[:space:]]+- $operation$" "$WORKFLOW" || fail "missing workflow operation: $operation"
done

printf '%s\n' 'qa-invite isolated deployment contract holds'
