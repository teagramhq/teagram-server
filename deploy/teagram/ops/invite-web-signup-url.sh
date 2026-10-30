#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly ROOT=/opt/teagram
readonly ENV_FILE="$ROOT/.env"
readonly STATE_DIR="$ROOT/.state"
readonly ENV_RECORD="$STATE_DIR/rollout-env"
readonly TARGET_NAME=TG_INVITE_WEB_SIGNUP_URL
readonly TARGET_VALUE=https://web.teagram.io/
readonly TARGET_LINE="$TARGET_NAME=$TARGET_VALUE"
readonly DESKTOP_NAME=TG_INVITE_DESKTOP_URL

mode=apply
case "$#" in
  0) ;;
  1)
    case "$1" in
      --check) mode=check ;;
      --rollback) mode=rollback ;;
      *) printf '%s\n' 'usage: invite-web-signup-url.sh [--check|--rollback]' >&2; exit 64 ;;
    esac
    ;;
  *) printf '%s\n' 'usage: invite-web-signup-url.sh [--check|--rollback]' >&2; exit 64 ;;
esac

fail() {
  printf 'invite-web-signup-url: %s\n' "$1" >&2
  exit 1
}

[[ "$EUID" == 0 ]] || fail 'must run as root'
exec 9>/tmp/teagram-deploy.lock || fail 'cannot open shared deployment lock'
flock -x 9 || fail 'cannot acquire shared deployment lock'
[[ -d "$ROOT" && ! -L "$ROOT" ]] || fail 'server checkout is not a real directory'
[[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] || fail '.env is not a regular file'
[[ "$(stat -c '%u' -- "$ENV_FILE")" == 0 ]] || fail '.env is not root-owned'
[[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || fail 'deployment state is not a real directory'
[[ "$(stat -c '%u' -- "$STATE_DIR")" == 0 ]] || fail 'deployment state directory is not root-owned'
state_mode=$(stat -c '%a' -- "$STATE_DIR") || fail 'cannot read deployment state directory mode'
[[ "$state_mode" =~ ^[0-7]{3,4}$ ]] || fail 'deployment state directory mode is invalid'
(( (8#$state_mode & 0022) == 0 )) || fail 'deployment state directory is writable by group or others'
[[ -f "$ENV_RECORD" && ! -L "$ENV_RECORD" ]] || fail 'verified .env record is not a regular file'
[[ "$(stat -c '%u:%a' -- "$ENV_RECORD")" == 0:600 ]] || fail 'verified .env record is not root-owned mode 0600'

record=$(cat -- "$ENV_RECORD") || fail 'cannot read verified .env record'
[[ "$record" =~ ^env_sha256=([0-9a-f]{64})\ source_sha=([0-9a-f]{40})$ ]] || fail 'verified .env record is malformed'
recorded_env_sha=${BASH_REMATCH[1]}
recorded_source_sha=${BASH_REMATCH[2]}
current_env_sha=$(sha256sum -- "$ENV_FILE") || fail 'cannot hash .env'
current_env_sha=${current_env_sha%% *}
[[ "$current_env_sha" == "$recorded_env_sha" ]] || fail '.env differs from its last verified rollout record'

env_mode=$(stat -c '%a' -- "$ENV_FILE") || fail 'cannot read .env mode'
env_group=$(stat -c '%g' -- "$ENV_FILE") || fail 'cannot read .env group'
web_count=$(grep -c "^${TARGET_NAME}=" "$ENV_FILE" || true)
desktop_count=$(grep -c "^${DESKTOP_NAME}=" "$ENV_FILE" || true)
[[ "$desktop_count" == 0 ]] || fail "$DESKTOP_NAME must remain unset"
[[ "$web_count" -le 1 ]] || fail "$TARGET_NAME is duplicated in .env"

if [[ "$web_count" == 1 ]]; then
  existing_line=$(grep -F "${TARGET_NAME}=" "$ENV_FILE") || fail "cannot read $TARGET_NAME"
  [[ "$existing_line" == "$TARGET_LINE" ]] || fail "$TARGET_NAME already has a different value"
fi

case "$mode" in
  check)
    if [[ "$web_count" == 0 ]]; then
      printf 'preflight=would_apply key=%s value=%s desktop=unset\n' "$TARGET_NAME" "$TARGET_VALUE"
    else
      printf 'preflight=already_applied key=%s value=%s desktop=unset\n' "$TARGET_NAME" "$TARGET_VALUE"
    fi
    exit 0
    ;;
  apply)
    if [[ "$web_count" == 1 ]]; then
      printf 'env_update=already_applied key=%s value=%s desktop=unset\n' "$TARGET_NAME" "$TARGET_VALUE"
      exit 0
    fi
    ;;
  rollback)
    if [[ "$web_count" == 0 ]]; then
      printf 'rollback=already_absent key=%s desktop=unset\n' "$TARGET_NAME"
      exit 0
    fi
    ;;
esac

work_dir=$(mktemp -d "$STATE_DIR/.invite-web-signup-url.XXXXXXXX") || fail 'cannot create private operation directory'
env_replaced=0
record_replaced=0
keep_work_dir=0

cleanup() {
  local status=$?
  set +e
  trap - EXIT

  if [[ "$status" != 0 ]]; then
    if [[ "$record_replaced" == 1 && -f "$work_dir/record.backup" ]]; then
      mv -f -- "$work_dir/record.backup" "$ENV_RECORD" || keep_work_dir=1
    fi
    if [[ "$env_replaced" == 1 && -f "$work_dir/env.backup" ]]; then
      mv -f -- "$work_dir/env.backup" "$ENV_FILE" || keep_work_dir=1
    fi
  fi

  if [[ "$keep_work_dir" == 1 ]]; then
    printf 'invite-web-signup-url: recovery files retained at %s\n' "$work_dir" >&2
  else
    rm -f -- "$work_dir/env.new" "$work_dir/record.new" \
      "$work_dir/env.backup" "$work_dir/record.backup" \
      "$work_dir/env.without-target" "$work_dir/env.original-without-target"
    if ! rmdir -- "$work_dir"; then
      printf 'invite-web-signup-url: could not remove operation directory %s\n' "$work_dir" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

cp -- "$ENV_FILE" "$work_dir/env.backup" || fail 'cannot stage .env rollback copy'
cp -- "$ENV_RECORD" "$work_dir/record.backup" || fail 'cannot stage rollout record rollback copy'
chgrp "$env_group" "$work_dir/env.backup" || fail 'cannot preserve .env rollback group'
chmod "$env_mode" -- "$work_dir/env.backup" || fail 'cannot preserve .env rollback mode'
chmod 600 -- "$work_dir/record.backup" || fail 'cannot secure rollout record rollback copy'

if [[ "$mode" == apply ]]; then
  cp -- "$ENV_FILE" "$work_dir/env.new" || fail 'cannot stage updated .env'
  if [[ -s "$work_dir/env.new" ]] && [[ "$(tail -c 1 -- "$work_dir/env.new" | wc -l)" == 0 ]]; then
    printf '\n' >> "$work_dir/env.new"
  fi
  printf '%s\n' "$TARGET_LINE" >> "$work_dir/env.new"
else
  sed "/^${TARGET_NAME}=/d" "$ENV_FILE" > "$work_dir/env.new" || fail 'cannot stage rollback .env'
fi

sed "/^${TARGET_NAME}=/d" "$work_dir/env.new" > "$work_dir/env.without-target" || fail 'cannot verify updated .env'
sed "/^${TARGET_NAME}=/d" "$ENV_FILE" > "$work_dir/env.original-without-target" || fail 'cannot verify original .env'
cmp -s -- "$work_dir/env.without-target" "$work_dir/env.original-without-target" || \
  fail 'operation would change an unrelated .env entry'

if [[ "$mode" == apply ]]; then
  [[ "$(grep -Fxc -- "$TARGET_LINE" "$work_dir/env.new" || true)" == 1 ]] || fail 'staged .env does not contain exactly one plain target assignment'
  [[ "$(grep -c "^${DESKTOP_NAME}=" "$work_dir/env.new" || true)" == 0 ]] || fail "$DESKTOP_NAME must remain unset"
else
  [[ "$(grep -c "^${TARGET_NAME}=" "$work_dir/env.new" || true)" == 0 ]] || fail 'rollback did not remove the target assignment'
fi

chgrp "$env_group" "$work_dir/env.new" || fail 'cannot preserve .env group'
chmod "$env_mode" -- "$work_dir/env.new" || fail 'cannot preserve .env mode'
new_env_sha=$(sha256sum -- "$work_dir/env.new") || fail 'cannot hash staged .env'
new_env_sha=${new_env_sha%% *}
printf 'env_sha256=%s source_sha=%s\n' "$new_env_sha" "$recorded_source_sha" > "$work_dir/record.new" || fail 'cannot stage verified .env record'
chmod 600 -- "$work_dir/record.new" || fail 'cannot secure verified .env record'
sync || fail 'cannot flush staged configuration'

mv -f -- "$work_dir/env.new" "$ENV_FILE" || fail 'cannot install updated .env'
env_replaced=1
mv -f -- "$work_dir/record.new" "$ENV_RECORD" || fail 'cannot update verified .env record'
record_replaced=1
sync || fail 'cannot flush updated configuration'

verified_env_sha=$(sha256sum -- "$ENV_FILE") || fail 'cannot verify updated .env'
verified_env_sha=${verified_env_sha%% *}
[[ "$verified_env_sha" == "$new_env_sha" ]] || fail 'updated .env digest does not match the staged record'
verified_record=$(cat -- "$ENV_RECORD") || fail 'cannot verify updated rollout record'
[[ "$verified_record" == "env_sha256=$new_env_sha source_sha=$recorded_source_sha" ]] || fail 'updated rollout record does not match .env'

if [[ "$mode" == apply ]]; then
  printf 'env_update=added key=%s value=%s desktop=unset\n' "$TARGET_NAME" "$TARGET_VALUE"
else
  printf 'rollback=removed key=%s desktop=unset\n' "$TARGET_NAME"
fi
