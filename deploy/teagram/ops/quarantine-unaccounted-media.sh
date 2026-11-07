#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly BOOTSTRAP_REPO=/opt/teagram
readonly QUARANTINE_BASE=/var/lib/teagram-plaintext-quarantine
[[ "$(id -u)" == 0 ]] || { printf '%s\n' 'media quarantine requires root' >&2; exit 1; }
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || { printf '%s\n' 'reviewed source SHA is invalid' >&2; exit 1; }
[[ "$#" == 1 && ( "$1" == --check || "$1" == --apply ) || \
   "$#" == 2 && "$1" == --restore && "$2" =~ ^[0-9]{8}T[0-9]{6}Z-[0-9]+-[0-9a-f]{7}$ ]] || {
  printf '%s\n' 'usage: quarantine-unaccounted-media.sh --check | --apply | --restore <run-id>' >&2
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
quarantine_exit() {
  local status=$?
  trap - EXIT
  remove_operation_temp || status=1
  if [[ "$status" != 0 && "${MODE:-check}" != check ]]; then
    printf 'quarantine_run_id=%s quarantine_path=%s\n' \
      "${TARGET_RUN_ID:-unavailable}" "${QUARANTINE_DIR:-unavailable}" >&2
  fi
  exit "$status"
}
trap quarantine_exit EXIT

git -C "$BOOTSTRAP_REPO" show "$OPS_SHA:deploy/teagram/ops/blob-encryption-common.inc" \
  > "$OPS_TEMP_DIR/common.inc" || { printf '%s\n' 'cannot stage the reviewed media operation helpers' >&2; exit 1; }
git -C "$BOOTSTRAP_REPO" show "$OPS_SHA:deploy/teagram/ops/quarantine_unaccounted_media.py" \
  > "$OPS_TEMP_DIR/quarantine-media.py" || { printf '%s\n' 'cannot stage the reviewed media quarantine helper' >&2; exit 1; }
chmod 600 -- "$OPS_TEMP_DIR/common.inc" "$OPS_TEMP_DIR/quarantine-media.py" || {
  printf '%s\n' 'cannot protect reviewed media quarantine helpers' >&2
  exit 1
}
# shellcheck disable=SC1091
source "$OPS_TEMP_DIR/common.inc"

MODE=apply
if [[ "$1" == --check ]]; then MODE=check; fi
if [[ "$1" == --restore ]]; then MODE=restore; fi
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-${OPS_SHA:0:7}"
TARGET_RUN_ID=$RUN_ID
if [[ "$MODE" == restore ]]; then TARGET_RUN_ID=$2; fi
QUARANTINE_DIR="$QUARANTINE_BASE/MAIN-3441-$TARGET_RUN_ID"

exec 9>/tmp/teagram-deploy.lock || ops_fail 'cannot open the shared deployment lock'
flock -x 9 || ops_fail 'cannot acquire the shared deployment lock'
ensure_evidence_root
validate_live_stack
validate_source_and_runtime
stage_runtime_helpers
configure_compose
verify_authority encrypted
verify_encrypted_inventory
check_progress_journal

INVENTORY_SHA=$(jq -er '.evidence.encrypted_inventory_sha256 | select(test("^[0-9a-f]{64}$"))' \
  "$AUTHORITY_DIR/mode.json") || ops_fail 'encrypted inventory digest is invalid'
AUTHORITY_SHA_BEFORE=$(sha256_file_contents "$AUTHORITY_DIR/mode.json") || ops_fail 'cannot fingerprint encrypted authority'
S3_PREFIX=$(jq -r '((.services.teagramd.environment // {}).TG_BLOB_S3_PREFIX // "teagramd") | select(type == "string")' \
  "$OPS_TEMP_DIR/compose.json") || ops_fail 'live S3 prefix is invalid'
[[ -z "$S3_PREFIX" || "$S3_PREFIX" =~ ^[A-Za-z0-9._/-]+$ ]] || ops_fail 'live S3 prefix is unsupported'
[[ "$S3_PREFIX" != /* && "$S3_PREFIX" != *..* ]] || ops_fail 'live S3 prefix is unsafe'
if [[ -n "$S3_PREFIX" && "$S3_PREFIX" != */ ]]; then S3_PREFIX="$S3_PREFIX/"; fi

container_env="$OPS_TEMP_DIR/teagramd-env.json"
container_env_stderr="$EVIDENCE_ROOT/media-quarantine-$RUN_ID-container-env.stderr"
[[ ! -e "$container_env_stderr" && ! -L "$container_env_stderr" ]] || ops_fail 'container inspection evidence path already exists'
if ! docker inspect --format '{{json .Config.Env}}' "$SERVER_CONTAINER" \
  > "$container_env" 2> "$container_env_stderr"; then
  chmod 600 -- "$container_env" "$container_env_stderr" || ops_fail 'cannot protect container inspection evidence'
  ops_fail 'cannot inspect the running teagramd environment'
fi
chmod 600 -- "$container_env" "$container_env_stderr" || ops_fail 'cannot protect container inspection evidence'
LIVE_S3_PREFIX=$(jq -r '
  [ .[] | strings | select(startswith("TG_BLOB_S3_PREFIX=")) | sub("^TG_BLOB_S3_PREFIX="; "") ] |
  if length == 1 then .[0] else error("S3 prefix setting is absent or duplicated") end
' "$container_env") || ops_fail 'running teagramd S3 prefix is invalid'
[[ -z "$LIVE_S3_PREFIX" || "$LIVE_S3_PREFIX" =~ ^[A-Za-z0-9._/-]+$ ]] || ops_fail 'running teagramd S3 prefix is unsupported'
[[ "$LIVE_S3_PREFIX" != /* && "$LIVE_S3_PREFIX" != *..* ]] || ops_fail 'running teagramd S3 prefix is unsafe'
if [[ -n "$LIVE_S3_PREFIX" && "$LIVE_S3_PREFIX" != */ ]]; then LIVE_S3_PREFIX="$LIVE_S3_PREFIX/"; fi
[[ "$LIVE_S3_PREFIX" == "$S3_PREFIX" ]] || ops_fail 'running teagramd S3 prefix differs from the live Compose render'

if [[ "$MODE" != check ]]; then
  if [[ ! -e "$QUARANTINE_BASE" && ! -L "$QUARANTINE_BASE" ]]; then
    mkdir -m 700 -- "$QUARANTINE_BASE" || ops_fail 'cannot create the private quarantine root'
  fi
  private_dir "$QUARANTINE_BASE" || ops_fail 'quarantine root is not root-owned mode 0700'
fi

mkdir -m 700 -- "$OPS_TEMP_DIR/mc-files" || ops_fail 'cannot create the private RustFS client mount'
printf '%s\0' "${COMPOSE[@]}" > "$OPS_TEMP_DIR/compose.argv" || ops_fail 'cannot stage the live Compose command'
chmod 600 -- "$OPS_TEMP_DIR/compose.argv" || ops_fail 'cannot protect the live Compose command'

HELPER_ARGS=(
  --mode "$MODE"
  --compose-argv "$OPS_TEMP_DIR/compose.argv"
  --prefix "$S3_PREFIX"
  --work-dir "$OPS_TEMP_DIR"
  --evidence-dir "$EVIDENCE_ROOT"
  --quarantine-base "$QUARANTINE_BASE"
  --run-id "$TARGET_RUN_ID"
  --diagnostic-id "$RUN_ID"
  --referenced-count "$INVENTORY_REFERENCED"
  --mount-dir "$OPS_TEMP_DIR/mc-files"
)
if [[ "$MODE" != restore ]]; then
  HELPER_ARGS+=(
    --inventory "$INVENTORY_REPORT_PATH"
    --inventory-sha256 "$INVENTORY_SHA"
    --journal "$REPORT_ROOT/${PROGRESS_PATH##*/}"
  )
fi
if [[ "$MODE" == apply ]]; then
  DELETE_AFTER=$(python3 -c 'from datetime import datetime, timedelta, timezone; print((datetime.now(timezone.utc) + timedelta(days=30)).date().isoformat())') || \
    ops_fail 'cannot calculate the quarantine retention date'
  HELPER_ARGS+=(--delete-after "$DELETE_AFTER")
fi
if [[ "$MODE" == restore ]]; then
  HELPER_ARGS[${#HELPER_ARGS[@]}-1]="$QUARANTINE_DIR"
fi

AUTHORITY_SHA_BEFORE_APPLY=$(sha256_file_contents "$AUTHORITY_DIR/mode.json") || \
  ops_fail 'cannot recheck encrypted authority before media quarantine'
[[ "$AUTHORITY_SHA_BEFORE_APPLY" == "$AUTHORITY_SHA_BEFORE" ]] || ops_fail 'encrypted authority changed before media quarantine'

if ! python3 "$OPS_TEMP_DIR/quarantine-media.py" "${HELPER_ARGS[@]}" \
  > "$OPS_TEMP_DIR/result.out"; then
  ops_fail 'quarantine operation failed; private diagnostics were retained'
fi
chmod 600 -- "$OPS_TEMP_DIR/result.out" || ops_fail 'cannot protect quarantine result'
verify_authority encrypted
AUTHORITY_SHA_AFTER=$(sha256_file_contents "$AUTHORITY_DIR/mode.json") || ops_fail 'cannot verify encrypted authority'
[[ "$AUTHORITY_SHA_AFTER" == "$AUTHORITY_SHA_BEFORE" ]] || ops_fail 'encrypted authority changed during media quarantine'
cat -- "$OPS_TEMP_DIR/result.out"
