#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly BOOTSTRAP_REPO=/opt/teagram
[[ "$(id -u)" == 0 ]] || { printf '%s\n' 'media cleanup requires root' >&2; exit 1; }
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || { printf '%s\n' 'reviewed source SHA is invalid' >&2; exit 1; }
[[ "$#" == 1 && "$1" == --acceptance-passed ]] || {
  printf '%s\n' 'usage: cleanup-media.sh --acceptance-passed' >&2
  exit 64
}
[[ -d "$BOOTSTRAP_REPO" && ! -L "$BOOTSTRAP_REPO" ]] || { printf '%s\n' 'server checkout is unavailable' >&2; exit 1; }

OPS_TEMP_DIR=$(mktemp -d /tmp/teagram-media-op.XXXXXXXX) || { printf '%s\n' 'cannot create private operation directory' >&2; exit 1; }
[[ -d "$OPS_TEMP_DIR" && ! -L "$OPS_TEMP_DIR" && "$(stat -c '%u:%a' -- "$OPS_TEMP_DIR")" == "0:700" ]] || {
  printf '%s\n' 'private operation directory failed validation' >&2
  exit 1
}
remove_operation_temp() {
  local path=${OPS_TEMP_DIR:-} resolved
  case "$path" in
    /tmp/teagram-media-op.????????)
      [[ -d "$path" && ! -L "$path" && "$(stat -c '%u:%a' -- "$path")" == "0:700" ]] || return 1
      resolved=$(readlink -f -- "$path") || return 1
      [[ "$resolved" == "$path" ]] || return 1
      rm -rf -- "$resolved"
      ;;
    *) return 1 ;;
  esac
}
bootstrap_exit() {
  local status=$?
  trap - EXIT
  remove_operation_temp || status=1
  exit "$status"
}
trap bootstrap_exit EXIT
git -C "$BOOTSTRAP_REPO" show "$OPS_SHA:deploy/teagram/ops/blob-encryption-common.inc" \
  > "$OPS_TEMP_DIR/common.inc" || { printf '%s\n' 'cannot stage the reviewed media operation helpers' >&2; exit 1; }
chmod 600 -- "$OPS_TEMP_DIR/common.inc" || { printf '%s\n' 'cannot protect reviewed media operation helpers' >&2; exit 1; }
# shellcheck disable=SC1091
source "$OPS_TEMP_DIR/common.inc"

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-${OPS_SHA:0:7}"
SERVER_STOPPED=0
RUSTFS_STOPPED=0

recover_runtime() {
  local authority_json outcome container_state health healthy
  if [[ "$RUSTFS_STOPPED" == 1 ]]; then
    container_state=$(docker inspect --format '{{.State.Status}}' "$RUSTFS_CONTAINER") || return 1
    if [[ "$container_state" != running ]]; then
      docker start "$RUSTFS_CONTAINER" >/dev/null || return 1
    fi
    healthy=0
    for _ in {1..60}; do
      health=$(docker inspect --format '{{.State.Health.Status}}' "$RUSTFS_CONTAINER") || return 1
      if [[ "$health" == healthy ]]; then healthy=1; break; fi
      sleep 1
    done
    [[ "$healthy" == 1 ]] || return 1
    RUSTFS_STOPPED=0
  fi
  if [[ "$SERVER_STOPPED" == 1 ]]; then
    authority_json=$(cd "$REPO" && python3 "$OPS_TEMP_DIR/blob-mode-state.py" authority \
      --state-dir "$AUTHORITY_DIR" --report-root "$REPORT_ROOT") || return 1
    outcome=$(jq -er '.outcome' <<< "$authority_json") || return 1
    [[ "$outcome" == encrypted ]] || return 1
    container_state=$(docker inspect --format '{{.State.Status}}' "$SERVER_CONTAINER") || return 1
    if [[ "$container_state" != running ]]; then
      docker start "$SERVER_CONTAINER" >/dev/null || return 1
    fi
    for _ in {1..60}; do
      container_state=$(docker inspect --format '{{.State.Status}}' "$SERVER_CONTAINER") || return 1
      if [[ "$container_state" == running ]]; then
        SERVER_STOPPED=0
        return 0
      fi
      sleep 1
    done
    return 1
  fi
  return 0
}

cleanup_exit() {
  local status=$?
  trap - EXIT
  set +e
  if ! recover_runtime; then
    printf '%s\n' 'runtime recovery failed; inspect the live containers before resuming' >&2
    status=1
  fi
  remove_operation_temp || status=1
  exit "$status"
}
trap cleanup_exit EXIT

exec 9>/tmp/teagram-deploy.lock || ops_fail 'cannot open the shared deployment lock'
flock -x 9 || ops_fail 'cannot acquire the shared deployment lock'
ensure_evidence_root
validate_live_stack
validate_source_and_runtime
stage_runtime_helpers
configure_compose
check_media_key
verify_authority encrypted
verify_encrypted_inventory
latest_backup_manifest
check_progress_journal

inspect_container "$SERVER_CONTAINER" teagramd
[[ "$INSPECT_STATE" == running ]] || ops_fail 'teagramd must be running before plaintext cleanup'
SERVER_STOPPED=1
docker stop --time 120 "$SERVER_CONTAINER" >/dev/null || ops_fail 'could not gracefully stop teagramd writers'
inspect_container "$SERVER_CONTAINER" teagramd
[[ "$INSPECT_STATE" == exited ]] || ops_fail 'teagramd writers did not stop'

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-${OPS_SHA:0:7}"
pre_cleanup_result="$EVIDENCE_ROOT/encryption-pre-cleanup-$RUN_ID.json"
pre_cleanup_stderr="$EVIDENCE_ROOT/encryption-pre-cleanup-$RUN_ID.stderr"
run_migration_job encrypt "$pre_cleanup_result" "$pre_cleanup_stderr"
verify_authority encrypted
require_no_unaccounted_objects "$pre_cleanup_result"
verified_references=$(jq -er '.objects | select(type == "number" and . >= 0)' "$pre_cleanup_result") || \
  ops_fail 'pre-cleanup reference count is invalid'

cleanup_result="$EVIDENCE_ROOT/plaintext-cleanup-$RUN_ID.json"
cleanup_stderr="$EVIDENCE_ROOT/plaintext-cleanup-$RUN_ID.stderr"
run_migration_job cleanup "$cleanup_result" "$cleanup_stderr"
removed=$(jq -er '.plaintext_objects_removed // 0 | select(type == "number" and . >= 0)' "$cleanup_result") || \
  ops_fail 'plaintext cleanup count is invalid'
cleanup_recheck="$EVIDENCE_ROOT/plaintext-cleanup-recheck-$RUN_ID.json"
cleanup_recheck_stderr="$EVIDENCE_ROOT/plaintext-cleanup-recheck-$RUN_ID.stderr"
run_migration_job cleanup "$cleanup_recheck" "$cleanup_recheck_stderr"
recheck_removed=$(jq -er '.plaintext_objects_removed // 0 | select(type == "number" and . >= 0)' "$cleanup_recheck") || \
  ops_fail 'plaintext cleanup recheck count is invalid'
[[ "$recheck_removed" == 0 ]] || ops_fail 'referenced plaintext objects remained after cleanup verification'

verify_authority encrypted
post_cleanup_result="$EVIDENCE_ROOT/encryption-post-cleanup-$RUN_ID.json"
post_cleanup_stderr="$EVIDENCE_ROOT/encryption-post-cleanup-$RUN_ID.stderr"
run_migration_job encrypt "$post_cleanup_result" "$post_cleanup_stderr"
require_no_unaccounted_objects "$post_cleanup_result"
post_cleanup_references=$(jq -er '.objects | select(type == "number" and . >= 0)' "$post_cleanup_result") || \
  ops_fail 'post-cleanup reference count is invalid'
[[ "$post_cleanup_references" == "$verified_references" ]] || ops_fail 'media references changed while writers were stopped'

restart_rustfs
post_restart_result="$EVIDENCE_ROOT/encryption-after-rustfs-restart-$RUN_ID.json"
post_restart_stderr="$EVIDENCE_ROOT/encryption-after-rustfs-restart-$RUN_ID.stderr"
run_migration_job encrypt "$post_restart_result" "$post_restart_stderr"
post_restart_references=$(jq -er '.objects | select(type == "number" and . >= 0)' "$post_restart_result") || \
  ops_fail 'post-RustFS-restart reference count is invalid'
[[ "$post_restart_references" == "$verified_references" ]] || ops_fail 'media references changed across RustFS restart'

smoke_log="$EVIDENCE_ROOT/mtproto-smoke-cleanup-$RUN_ID.log"
[[ ! -e "$smoke_log" && ! -L "$smoke_log" ]] || ops_fail 'MTProto smoke evidence path already exists'
start_server_and_smoke "$smoke_log"

printf 'cleanup=pass authority=encrypted image_sha=%s dump_path=%s key_backup=%s current_references=%s authority_inventory_references=%s encrypted_envelopes=%s plaintext_referenced_before_cleanup=%s no_valid_envelopes=%s plaintext_objects_removed=%s plaintext_referenced_after_cleanup=0 cleanup_recheck_removed=%s rustfs_restart=pass teagramd_restart=pass encrypted_readback=pass mtproto_smoke=pass evidence=%s\n' \
  "$LIVE_IMAGE_SHA" "$BACKUP_DUMP" "$KEY_BACKUP" "$verified_references" "$INVENTORY_REFERENCED" \
  "$INVENTORY_ENCRYPTED" "$INVENTORY_PLAINTEXT" "$INVENTORY_NO_VALID" "$removed" \
  "$recheck_removed" "$EVIDENCE_ROOT"
