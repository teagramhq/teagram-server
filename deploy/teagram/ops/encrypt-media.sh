#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly BOOTSTRAP_REPO=/opt/teagram
[[ "$(id -u)" == 0 ]] || { printf '%s\n' 'media encryption requires root' >&2; exit 1; }
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || { printf '%s\n' 'reviewed source SHA is invalid' >&2; exit 1; }
[[ "$#" == 0 ]] || { printf '%s\n' 'usage: encrypt-media.sh' >&2; exit 64; }
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
DUMP_TMP=
MANIFEST_TMP=
RESTORE_CONTAINER=
RESTORE_CONTAINER_STARTED=0
SERVER_STOPPED=0
RUSTFS_STOPPED=0

recover_runtime() {
  local authority_json outcome container_state health healthy
  if [[ "$RUSTFS_STOPPED" == 1 ]]; then
    if docker inspect --format '{{.State.Status}}' "$RUSTFS_CONTAINER" >/dev/null 2>&1; then
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
    else
      return 1
    fi
  fi
  if [[ "$SERVER_STOPPED" == 1 ]]; then
    authority_json=$(cd "$REPO" && python3 "$OPS_TEMP_DIR/blob-mode-state.py" authority \
      --state-dir "$AUTHORITY_DIR" --report-root "$REPORT_ROOT") || return 1
    outcome=$(jq -er '.outcome' <<< "$authority_json") || return 1
    [[ "$outcome" == s3-accepted || "$outcome" == encrypted ]] || return 1
    container_state=$(docker inspect --format '{{.State.Status}}' "$SERVER_CONTAINER") || return 1
    if [[ "$container_state" != running ]]; then
      docker start "$SERVER_CONTAINER" >/dev/null || return 1
    fi
    for _ in {1..60}; do
      container_state=$(docker inspect --format '{{.State.Status}}' "$SERVER_CONTAINER") || return 1
      if [[ "$container_state" == running ]]; then
        restore_bot_api_inner optional "$SERVER_CONTAINER" "${COMPOSE[@]}" || return 1
        SERVER_STOPPED=0
        return 0
      fi
      sleep 1
    done
    return 1
  fi
  return 0
}

encrypt_exit() {
  local status=$?
  trap - EXIT
  set +e
  if [[ -n "$RESTORE_CONTAINER" && "$RESTORE_CONTAINER_STARTED" == 1 ]] && \
    docker inspect --type container "$RESTORE_CONTAINER" >/dev/null 2>&1; then
    restore_label=$(docker inspect --format '{{index .Config.Labels "com.teagram.ops.operation"}}' "$RESTORE_CONTAINER")
    if [[ "$restore_label" == media-backup ]]; then
      docker stop --time 15 "$RESTORE_CONTAINER" >/dev/null || status=1
    else
      printf '%s\n' 'refusing to stop an unowned PostgreSQL restore container' >&2
      status=1
    fi
  fi
  if [[ -n "$DUMP_TMP" && -f "$DUMP_TMP" && ! -L "$DUMP_TMP" ]]; then
    case "$DUMP_TMP" in
      /root/teagram-media-encryption/.pgdump-pre-migration.*) rm -f -- "$DUMP_TMP" || status=1 ;;
      *) printf '%s\n' 'refusing to remove an unexpected dump staging path' >&2; status=1 ;;
    esac
  fi
  if [[ -n "$MANIFEST_TMP" && -f "$MANIFEST_TMP" && ! -L "$MANIFEST_TMP" ]]; then
    case "$MANIFEST_TMP" in
      /root/teagram-media-encryption/.pgdump-manifest.????????) rm -f -- "$MANIFEST_TMP" || status=1 ;;
      *) printf '%s\n' 'refusing to remove an unexpected manifest staging path' >&2; status=1 ;;
    esac
  fi
  if ! recover_runtime; then
    printf '%s\n' 'runtime recovery failed; inspect the live containers before resuming' >&2
    status=1
  fi
  remove_operation_temp || status=1
  exit "$status"
}
trap encrypt_exit EXIT

exec 9>/tmp/teagram-deploy.lock || ops_fail 'cannot open the shared deployment lock'
flock -x 9 || ops_fail 'cannot acquire the shared deployment lock'
ensure_evidence_root
validate_live_stack
validate_source_and_runtime
stage_runtime_helpers
configure_compose
check_media_key

authority_json=$(cd "$REPO" && python3 "$OPS_TEMP_DIR/blob-mode-state.py" authority \
  --state-dir "$AUTHORITY_DIR" --report-root "$REPORT_ROOT") || ops_fail 'blob authority verification failed'
current_authority=$(jq -er '.outcome' <<< "$authority_json") || ops_fail 'blob authority outcome is invalid'
case "$current_authority" in
  encrypted)
    verify_encrypted_inventory
    latest_backup_manifest
    check_progress_journal
    inspect_container "$SERVER_CONTAINER" teagramd
    [[ "$INSPECT_STATE" == running ]] || ops_fail 'teagramd must be running before encrypted-media verification'
    SERVER_STOPPED=1
    docker stop --time 120 "$SERVER_CONTAINER" >/dev/null || ops_fail 'could not gracefully stop teagramd writers'
    inspect_container "$SERVER_CONTAINER" teagramd
    [[ "$INSPECT_STATE" == exited ]] || ops_fail 'teagramd writers did not stop'

    RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-${OPS_SHA:0:7}"
    existing_result="$EVIDENCE_ROOT/encryption-existing-$RUN_ID.json"
    existing_stderr="$EVIDENCE_ROOT/encryption-existing-$RUN_ID.stderr"
    run_migration_job encrypt "$existing_result" "$existing_stderr"
    require_no_unaccounted_objects "$existing_result"
    existing_objects=$(jq -er '.objects | select(type == "number" and . >= 0)' "$existing_result") || \
      ops_fail 'existing encrypted reference count is invalid'
    restart_rustfs
    existing_restart_result="$EVIDENCE_ROOT/encryption-existing-rustfs-restart-$RUN_ID.json"
    existing_restart_stderr="$EVIDENCE_ROOT/encryption-existing-rustfs-restart-$RUN_ID.stderr"
    run_migration_job encrypt "$existing_restart_result" "$existing_restart_stderr"
    require_no_unaccounted_objects "$existing_restart_result"
    existing_restart_objects=$(jq -er '.objects | select(type == "number" and . >= 0)' "$existing_restart_result") || \
      ops_fail 'post-restart encrypted reference count is invalid'
    [[ "$existing_restart_objects" == "$existing_objects" ]] || \
      ops_fail 'media references changed while writers were stopped'
    verify_encrypted_inventory
    smoke_log="$EVIDENCE_ROOT/mtproto-smoke-existing-$RUN_ID.log"
    [[ ! -e "$smoke_log" && ! -L "$smoke_log" ]] || ops_fail 'MTProto smoke evidence path already exists'
    start_server_and_smoke "$smoke_log"
    printf 'migration=already_encrypted authority=encrypted image_sha=%s dump_restore=verified dump_path=%s key_backup=%s current_references=%s authority_references=%s valid_envelopes=%s no_valid_envelopes=%s rustfs_restart=pass teagramd=running smoke=pass\n' \
      "$LIVE_IMAGE_SHA" "$BACKUP_DUMP" "$KEY_BACKUP" "$existing_objects" "$INVENTORY_REFERENCED" \
      "$INVENTORY_ENCRYPTED" "$INVENTORY_NO_VALID"
    exit 0
    ;;
  s3-accepted) ;;
  *) ops_fail 'live authority is not accepted plaintext S3' ;;
esac

inspect_container "$SERVER_CONTAINER" teagramd
[[ "$INSPECT_STATE" == running ]] || ops_fail 'teagramd must be running before migration'
SERVER_STOPPED=1
docker stop --time 120 "$SERVER_CONTAINER" >/dev/null || ops_fail 'could not gracefully stop teagramd writers'
inspect_container "$SERVER_CONTAINER" teagramd
[[ "$INSPECT_STATE" == exited ]] || ops_fail 'teagramd writers did not stop'

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$-${OPS_SHA:0:7}"
dump_path="$EVIDENCE_ROOT/pgdump-pre-migration-$RUN_ID.dump"
manifest_path="$EVIDENCE_ROOT/pgdump-pre-migration-$RUN_ID.manifest"
dump_stderr="$EVIDENCE_ROOT/pgdump-pre-migration-$RUN_ID.stderr"
restore_stderr="$EVIDENCE_ROOT/pgrestore-pre-migration-$RUN_ID.stderr"
restore_start="$EVIDENCE_ROOT/pgrestore-pre-migration-$RUN_ID.start.stderr"
for path in "$dump_path" "$manifest_path" "$dump_stderr" "$restore_stderr" "$restore_start"; do
  [[ ! -e "$path" && ! -L "$path" ]] || ops_fail 'database backup evidence path already exists'
done
inspect_container "$POSTGRES_CONTAINER" postgres
PG_IMAGE_ID=$INSPECT_IMAGE
[[ "$PG_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || ops_fail 'Postgres image identity is invalid'
DUMP_TMP=$(mktemp "$EVIDENCE_ROOT/.pgdump-pre-migration.XXXXXXXX") || ops_fail 'cannot allocate database dump evidence'
if docker exec -i "$POSTGRES_CONTAINER" pg_dump --format=custom --no-owner --no-privileges \
  -U postgres -d teagram </dev/null > "$DUMP_TMP" 2> "$dump_stderr"; then
  chmod 600 -- "$dump_stderr" || ops_fail 'cannot protect database dump diagnostics'
else
  chmod 600 -- "$dump_stderr" || true
  ops_fail "pg_dump failed; private diagnostic retained at $dump_stderr"
fi
[[ -s "$DUMP_TMP" ]] || ops_fail 'pg_dump produced an empty archive'
chmod 600 -- "$DUMP_TMP" || ops_fail 'cannot protect database dump'

RESTORE_CONTAINER="teagram-media-restore-$RUN_ID"
[[ "$RESTORE_CONTAINER" =~ ^teagram-media-restore-[0-9]{8}T[0-9]{6}Z-[0-9]+-[0-9a-f]{7}$ ]] || \
  ops_fail 'isolated restore container name is invalid'
if docker inspect --type container "$RESTORE_CONTAINER" >/dev/null 2>&1; then
  ops_fail 'isolated restore container name already exists'
fi
if docker run --detach --rm --name "$RESTORE_CONTAINER" --network none --log-driver none \
  --label com.teagram.ops.operation=media-backup \
  --tmpfs /var/lib/postgresql:rw,nosuid,nodev \
  --env PGDATA=/var/lib/postgresql/18/docker \
  --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=teagram \
  "$PG_IMAGE_ID" >/dev/null 2> "$restore_start"; then
  RESTORE_CONTAINER_STARTED=1
else
  chmod 600 -- "$restore_start" || true
  ops_fail "cannot start isolated dump restore; private diagnostic retained at $restore_start"
fi
chmod 600 -- "$restore_start" || ops_fail 'cannot protect isolated restore diagnostics'
ready=0
for _ in {1..60}; do
  if docker exec "$RESTORE_CONTAINER" pg_isready -h 127.0.0.1 -U postgres -d teagram >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
[[ "$ready" == 1 ]] || ops_fail 'isolated PostgreSQL restore did not become ready'
if docker exec -i "$RESTORE_CONTAINER" pg_restore --list - < "$DUMP_TMP" \
  > "$OPS_TEMP_DIR/pgrestore-list.txt" 2> "$restore_stderr"; then
  :
else
  chmod 600 -- "$restore_stderr" || true
  ops_fail "pg_restore archive validation failed; private diagnostic retained at $restore_stderr"
fi
[[ -s "$OPS_TEMP_DIR/pgrestore-list.txt" ]] || ops_fail 'pg_restore archive has no table of contents'
if docker exec -i "$RESTORE_CONTAINER" pg_restore --exit-on-error --no-owner --no-privileges \
  -U postgres -d teagram - < "$DUMP_TMP" >/dev/null 2>> "$restore_stderr"; then
  :
else
  chmod 600 -- "$restore_stderr" || true
  ops_fail "isolated database restore failed; private diagnostic retained at $restore_stderr"
fi
table_count=$(docker exec "$RESTORE_CONTAINER" psql -X -Atq -U postgres -d teagram \
  -c "SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname = 'public'") || \
  ops_fail 'cannot verify isolated database restore'
[[ "$table_count" =~ ^[1-9][0-9]*$ ]] || ops_fail 'isolated database restore contains no public tables'
chmod 600 -- "$restore_stderr" || ops_fail 'cannot protect isolated restore diagnostics'
restore_label=$(docker inspect --format '{{index .Config.Labels "com.teagram.ops.operation"}}' "$RESTORE_CONTAINER") || \
  ops_fail 'cannot validate isolated restore container ownership'
[[ "$restore_label" == media-backup ]] || ops_fail 'isolated restore container ownership changed'
docker stop --time 15 "$RESTORE_CONTAINER" >/dev/null || ops_fail 'cannot remove isolated restore container'
RESTORE_CONTAINER_STARTED=0

dump_sha=$(sha256sum -- "$DUMP_TMP") || ops_fail 'cannot hash the verified database dump'
dump_sha=${dump_sha%% *}
ln -- "$DUMP_TMP" "$dump_path" || ops_fail 'cannot publish the verified database dump without overwrite'
rm -- "$DUMP_TMP" || ops_fail 'cannot remove dump staging link'
DUMP_TMP=
sync || ops_fail 'cannot sync the verified database dump'
MANIFEST_TMP=$(mktemp "$EVIDENCE_ROOT/.pgdump-manifest.XXXXXXXX") || ops_fail 'cannot allocate backup manifest'
jq -n --arg dump_path "$dump_path" --arg dump_sha256 "$dump_sha" \
  --arg source_sha "$LIVE_IMAGE_SHA" --arg postgres_image "$PG_IMAGE_ID" \
  --arg verified_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg restore_tables "$table_count" \
  '{result:"isolated-restore-pass",dump_path:$dump_path,dump_sha256:$dump_sha256,
    source_sha:$source_sha,postgres_image:$postgres_image,verified_at_utc:$verified_at,
    restore_public_tables:($restore_tables|tonumber)}' > "$MANIFEST_TMP" || ops_fail 'cannot write database backup manifest'
chmod 600 -- "$MANIFEST_TMP" || ops_fail 'cannot protect database backup manifest'
ln -- "$MANIFEST_TMP" "$manifest_path" || ops_fail 'cannot publish backup manifest without overwrite'
rm -- "$MANIFEST_TMP" || ops_fail 'cannot remove backup manifest staging link'
MANIFEST_TMP=
sync || ops_fail 'cannot sync database backup manifest'

progress_file="$REPORT_ROOT/${PROGRESS_PATH##*/}"
if [[ -e "$progress_file" || -L "$progress_file" ]]; then
  private_file "$progress_file" || ops_fail 'existing migration progress journal is unsafe'
fi
migration_result="$EVIDENCE_ROOT/encryption-migration-$RUN_ID.json"
migration_stderr="$EVIDENCE_ROOT/encryption-migration-$RUN_ID.stderr"
run_migration_job encrypt "$migration_result" "$migration_stderr"
verify_authority encrypted
verify_encrypted_inventory
require_no_unaccounted_plaintext
require_no_unaccounted_objects "$migration_result"
job_objects=$(jq -er '.objects | select(type == "number" and . >= 0)' "$migration_result") || \
  ops_fail 'migration reference count is invalid'
[[ "$job_objects" == "$INVENTORY_REFERENCED" && "$INVENTORY_PLAINTEXT" == "$job_objects" ]] || \
  ops_fail 'migration before-and-after inventory counts do not agree'

restart_rustfs
post_restart_result="$EVIDENCE_ROOT/encryption-rustfs-restart-$RUN_ID.json"
post_restart_stderr="$EVIDENCE_ROOT/encryption-rustfs-restart-$RUN_ID.stderr"
run_migration_job encrypt "$post_restart_result" "$post_restart_stderr"
require_no_unaccounted_objects "$post_restart_result"
post_restart_objects=$(jq -er '.objects | select(type == "number" and . >= 0)' "$post_restart_result") || \
  ops_fail 'post-RustFS-restart reference count is invalid'
[[ "$post_restart_objects" == "$job_objects" ]] || ops_fail 'media references changed while writers were stopped'
verify_encrypted_inventory

smoke_log="$EVIDENCE_ROOT/mtproto-smoke-$RUN_ID.log"
[[ ! -e "$smoke_log" && ! -L "$smoke_log" ]] || ops_fail 'MTProto smoke evidence path already exists'
start_server_and_smoke "$smoke_log"

printf 'migration=pass authority=encrypted image_sha=%s dump_restore=pass dump_path=%s key_backup=%s referenced_before=%s plaintext_sources_before_cleanup=%s encrypted_after=%s no_valid_envelopes_after=%s cleanup=deferred rustfs_restart=pass teagramd_restart=pass mtproto_smoke=pass evidence=%s\n' \
  "$LIVE_IMAGE_SHA" "$dump_path" "$KEY_BACKUP" "$job_objects" "$INVENTORY_PLAINTEXT" \
  "$INVENTORY_ENCRYPTED" "$INVENTORY_NO_VALID" "$EVIDENCE_ROOT"
