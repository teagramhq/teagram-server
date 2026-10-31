# shellcheck shell=bash

restore_env() {
  local temporary backup_owner backup_group env_owner env_group backup_mode
  [[ -n "$ENV_BACKUP" && -f "$ENV_BACKUP" && ! -L "$ENV_BACKUP" ]] || return 1
  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] || return 1
  backup_owner=$(stat -c '%u' "$ENV_BACKUP") || return 1
  backup_group=$(stat -c '%g' "$ENV_BACKUP") || return 1
  backup_mode=$(stat -c '%a' "$ENV_BACKUP") || return 1
  env_owner=$(stat -c '%u' "$ENV_FILE") || return 1
  env_group=$(stat -c '%g' "$ENV_FILE") || return 1
  [[ "$backup_owner:$backup_group:$backup_mode" == "$env_owner:$env_group:600" ]] || return 1
  temporary=$(mktemp "$ENV_FILE.rollback.XXXXXXXX") || return 1
  if ! install -o "$env_owner" -g "$env_group" -m 0600 -- "$ENV_BACKUP" "$temporary" ||
    ! mv -f -- "$temporary" "$ENV_FILE"; then
    rm -f -- "$temporary"
    return 1
  fi
}

restore_previous_runtime() {
  local expected_flag=${1:?expected flag is required}
  local rollback_flag rollback_image_ref rollback_digest
  restore_env || return 1
  rollback_flag=$(active_flag 2>/dev/null || true)
  if [[ "$rollback_flag" != "$expected_flag" ]]; then
    rollback_image_ref=$(demo_active_image_ref) || return 1
    rollback_digest=${rollback_image_ref##*@}
    [[ "$rollback_image_ref" == "$DEMO_IMAGE_REPOSITORY@$rollback_digest" &&
      "$rollback_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
    run_rollout "$rollback_digest" || return 1
    rollback_flag=$(active_flag) || return 1
  fi
  [[ "$rollback_flag" == "$expected_flag" ]] || return 1
  ROLLBACK_CONFIRMED=1
  printf 'rollback=confirmed effective_flag=%s environment_backup=%s\n' \
    "$rollback_flag" "$ENV_BACKUP" >&2
}

turnstile_cleanup() {
  local status=$?
  trap - EXIT
  trap '' HUP INT TERM
  if [[ -n "$ENV_BACKUP" && -f "$ENV_BACKUP" && ! -L "$ENV_BACKUP" &&
    "$ROLLOUT_COMMITTED" != 1 && "$ROLLBACK_CONFIRMED" != 1 ]]; then
    if ! restore_previous_runtime "$expected_flag"; then
      printf 'demo Turnstile rollout: uncommitted rollback failed environment_backup=%s\n' \
        "$ENV_BACKUP" >&2
      status=1
    fi
  fi
  if [[ -n "$TMP_DIR" ]]; then
    case "$TMP_DIR" in
      /tmp/teagram-demo-turnstile.????????) rm -rf -- "$TMP_DIR" || status=1 ;;
      *) printf '%s\n' 'demo Turnstile rollout: refusing unexpected temporary path' >&2; status=1 ;;
    esac
  fi
  exit "$status"
}

install_turnstile_traps() {
  trap turnstile_cleanup EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
}
