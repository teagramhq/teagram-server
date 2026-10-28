#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly REPO=/opt/teagram
readonly CHECKOUT=/home/restore-drill/teagram-server
readonly DRILL_USER=restore-drill
readonly DUMP_DIR=/root
readonly DEPLOY_LOCK=/tmp/teagram-deploy.lock
readonly OPERATION_LOCK=/run/teagram-restore-drill-operation.lock
readonly CONTAINER=teagram-teagramd-1
readonly MIN_BASE=aa40758600452b422cc10f99a662df8e39ac3ef3
readonly LOCAL_DUMP_BASE=8cc64750f467b782b81efaddd0077cc619c52113
readonly READINESS_URL=https://teagram.tailaa4918.ts.net/admin
readonly PREPARE_MIN_DISK_BYTES=$((4 * 1024 * 1024 * 1024))
readonly RUN_MIN_MEMORY_BYTES=$((2816 * 1024 * 1024))
readonly RUN_MIN_IDLE_PERCENT=10

preflight_failure() {
  printf '{"result":"fail","phase":"preflight","category":"%s","cleanup":"%s","exit":1}\n' "$1" "$2"
  exit 1
}

if [[ $# != 0 ]]; then
  printf '%s\n' 'usage: restore-drill.sh' >&2
  exit 64
fi
if [[ $(id -u) != 0 ]]; then preflight_failure wrong_user not_started; fi
if ! [[ ${OPS_SHA:-} =~ ^[0-9a-f]{40}$ ]]; then preflight_failure invalid_source not_started; fi
for required_sha in "$MIN_BASE" "$LOCAL_DUMP_BASE"; do
  if ! git -C "$REPO" cat-file -e "$required_sha^{commit}" 2>/dev/null ||
    ! git -C "$REPO" merge-base --is-ancestor "$required_sha" "$OPS_SHA"; then
    preflight_failure unsupported_source not_started
  fi
done
if ! git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" origin/main; then
  preflight_failure source_not_on_main not_started
fi

for tool in awk chmod cmp cp dirname flock git mktemp realpath rm sort stat tailscale timeout; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf 'host_tool_missing=%s\n' "$tool"
    preflight_failure missing_host_tool not_started
  fi
done
if [[ ! -x /usr/bin/python3 || ! -x /usr/bin/docker || ! -x /usr/bin/timeout ||
  ! -x /bin/bash || ! -x /bin/su ]] ||
  ! command -v curl >/dev/null 2>&1 || ! command -v id >/dev/null 2>&1; then
  for path in /usr/bin/python3 /usr/bin/docker /usr/bin/timeout /bin/bash /bin/su; do
    if [[ ! -x $path ]]; then printf 'host_tool_missing=%s\n' "$path"; break; fi
  done
  if ! command -v curl >/dev/null 2>&1; then printf '%s\n' 'host_tool_missing=curl';
  elif ! command -v id >/dev/null 2>&1; then printf '%s\n' 'host_tool_missing=id'; fi
  preflight_failure missing_host_tool not_started
fi
if [[ ! -d /home/restore-drill || -L /home/restore-drill || ! -d "$CHECKOUT" || -L "$CHECKOUT" ]]; then
  preflight_failure missing_restore_checkout not_started
fi
drill_uid=$(id -u "$DRILL_USER" 2>/dev/null) || preflight_failure missing_restore_user not_started
checkout_mode=$(stat -c '%a' -- "$CHECKOUT" 2>/dev/null) || preflight_failure invalid_restore_checkout not_started
if [[ $drill_uid == 0 || $(stat -c '%u' -- "$CHECKOUT" 2>/dev/null) != "$drill_uid" ||
  $(realpath -e -- "$CHECKOUT" 2>/dev/null) != "$CHECKOUT" ]] ||
  (( (8#$checkout_mode & 18) != 0 )); then
  preflight_failure invalid_restore_checkout not_started
fi
home_mode=$(stat -c '%a' -- /home/restore-drill 2>/dev/null) || preflight_failure invalid_restore_home not_started
if [[ $(stat -c '%u' -- /home/restore-drill 2>/dev/null) != "$drill_uid" ]] ||
  (( (8#$home_mode & 18) != 0 )); then
  preflight_failure invalid_restore_home not_started
fi

remove_work_directory() {
  local path=$1
  if [[ ! $path =~ ^/tmp/teagram-restore-drill\.[A-Za-z0-9]{8}$ ||
    -L $path || ! -d $path || $(dirname -- "$path") != /tmp ||
    $(realpath -e -- "$path" 2>/dev/null) != "$path" ||
    $(stat -c '%u:%a' -- "$path" 2>/dev/null) != 0:700 ]]; then
    return 1
  fi
  rm -rf -- "$path" >/dev/null 2>&1 || return 1
  [[ ! -e $path && ! -L $path ]]
}

cleanup_work() {
  local status=$?
  trap - EXIT
  if [[ -n ${work:-} ]] && ! remove_work_directory "$work"; then
    printf '%s\n' '{"result":"fail","phase":"cleanup","category":"host_work_cleanup_failed","cleanup":"unverified","exit":1}' >&2
    status=1
  fi
  exit "$status"
}

publish_final_output() {
  local summary_output=$1 report_output=$2 inventory_output=$3 final_status=$4
  if ! remove_work_directory "$work"; then
    trap - EXIT
    printf '%s\n' '{"result":"fail","phase":"cleanup","category":"host_work_cleanup_failed","cleanup":"unverified","exit":1}'
    exit 1
  fi
  work=
  trap - EXIT
  printf '%s\n' "$summary_output"
  printf '%s\n' "$report_output"
  printf '%s\n' 'dump_inventory_csv_begin'
  printf '%s\n' "$inventory_output"
  printf '%s\n' 'dump_inventory_csv_end'
  return "$final_status"
}
work=$(mktemp -d /tmp/teagram-restore-drill.XXXXXXXX 2>/dev/null) || preflight_failure temp_unavailable not_started
trap cleanup_work EXIT
if [[ ! $work =~ ^/tmp/teagram-restore-drill\.[A-Za-z0-9]{8}$ ||
  -L $work || ! -d $work || $(realpath -e -- "$work" 2>/dev/null) != "$work" ||
  $(stat -c '%u:%a' -- "$work" 2>/dev/null) != 0:700 ]]; then
  preflight_failure unsafe_temp_directory not_started
fi

as_drill() {
  local command quoted argument
  printf -v command 'cd %q && ' "$CHECKOUT"
  for argument in "$@"; do
    printf -v quoted '%q' "$argument"
    command+="$quoted "
  done
  command=${command% }
  /bin/su -l -s /bin/bash -c "$command" "$DRILL_USER"
}

checkout_is_clean() {
  local checkout=$1 status
  if ! status=$(as_drill git -C "$checkout" status --porcelain --untracked-files=all --ignored 2>/dev/null); then
    return 2
  fi
  [[ -z $status ]]
}

acquire_deploy_lock() {
  [[ -f $DEPLOY_LOCK && ! -L $DEPLOY_LOCK ]] || return 1
  exec {deploy_lock_fd}<"$DEPLOY_LOCK" || return 1
  if ! flock -s -n "$deploy_lock_fd"; then
    exec {deploy_lock_fd}<&-
    return 1
  fi
}

release_deploy_lock() {
  if [[ -n ${deploy_lock_fd:-} ]]; then
    flock -u "$deploy_lock_fd" || return 1
    exec {deploy_lock_fd}<&-
    unset deploy_lock_fd
  fi
}

acquire_operation_lock() {
  local lock_owner_mode expected_owner flock_status
  if [[ -L $OPERATION_LOCK || ( -e $OPERATION_LOCK && ! -f $OPERATION_LOCK ) ]]; then
    return 2
  fi
  exec {operation_lock_fd}>>"$OPERATION_LOCK" || return 2
  lock_owner_mode=$(stat -c '%u:%a' -- "$OPERATION_LOCK") || {
    exec {operation_lock_fd}>&-
    return 2
  }
  expected_owner=$(id -u) || {
    exec {operation_lock_fd}>&-
    return 2
  }
  if [[ $lock_owner_mode != "$expected_owner:600" ]]; then
    exec {operation_lock_fd}>&-
    return 2
  fi
  if flock -x -n "$operation_lock_fd"; then
    return 0
  else
    flock_status=$?
    exec {operation_lock_fd}>&-
    (( flock_status == 1 )) && return 1
    return 2
  fi
}

if acquire_operation_lock; then
  :
else
  operation_lock_status=$?
  case "$operation_lock_status" in
    1) preflight_failure drill_in_progress not_started ;;
    *) preflight_failure operation_lock_unavailable not_started ;;
  esac
fi

if ! acquire_deploy_lock; then preflight_failure rollout_in_progress not_started; fi
release_deploy_lock || preflight_failure rollout_lock_release_failed not_started

checkout_remote=$(as_drill git -C "$CHECKOUT" remote get-url origin 2>/dev/null) ||
  preflight_failure invalid_restore_checkout not_started
case "$checkout_remote" in
  https://github.com/teagramhq/teagram-server|https://github.com/teagramhq/teagram-server.git|git@github.com:teagramhq/teagram-server.git) ;;
  *) preflight_failure invalid_restore_remote not_started ;;
esac
if checkout_is_clean "$CHECKOUT"; then
  :
else
  checkout_status=$?
  case "$checkout_status" in
    2) preflight_failure restore_checkout_status_failed not_started ;;
    *) preflight_failure dirty_restore_checkout not_started ;;
  esac
fi
if ! as_drill git -C "$CHECKOUT" fetch --quiet origin main >/dev/null 2>&1 ||
  ! as_drill git -C "$CHECKOUT" merge-base --is-ancestor "$MIN_BASE" origin/main >/dev/null 2>&1 ||
  ! as_drill git -C "$CHECKOUT" merge-base --is-ancestor "$LOCAL_DUMP_BASE" origin/main >/dev/null 2>&1; then
  preflight_failure stale_restore_checkout not_started
fi

live_identity() {
  /usr/bin/docker inspect --format '{{.State.Running}}|{{.Id}}|{{.Image}}|{{.State.StartedAt}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$CONTAINER"
}

identity=$(live_identity 2>/dev/null) || preflight_failure live_container_unavailable not_started
IFS='|' read -r running container_id image_id started_at live_sha compose_files <<<"$identity"
if [[ $running != true || ! $container_id =~ ^[0-9a-f]{64}$ ||
  ! $image_id =~ ^sha256:[0-9a-f]{64}$ || -z $started_at ||
  ! $live_sha =~ ^[0-9a-f]{40}$ || -z $compose_files ]]; then
  preflight_failure invalid_live_identity not_started
fi
if ! git -C "$REPO" merge-base --is-ancestor "$MIN_BASE" "$live_sha" ||
  ! git -C "$REPO" merge-base --is-ancestor "$LOCAL_DUMP_BASE" "$live_sha" ||
  ! git -C "$REPO" merge-base --is-ancestor "$live_sha" origin/main ||
  ! git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" "$live_sha"; then
  preflight_failure live_commit_not_ready not_started
fi
if ! as_drill git -C "$CHECKOUT" checkout --detach "$live_sha" >/dev/null 2>&1 ||
  [[ $(as_drill git -C "$CHECKOUT" rev-parse HEAD 2>/dev/null) != "$live_sha" ]]; then
  preflight_failure source_checkout_failed not_started
fi
if checkout_is_clean "$CHECKOUT"; then
  :
else
  checkout_status=$?
  case "$checkout_status" in
    2) preflight_failure source_checkout_status_failed not_started ;;
    *) preflight_failure source_checkout_failed not_started ;;
  esac
fi
readonly HARNESS="$CHECKOUT/deploy/teagramd/rollout-runner/test-restore-readiness.sh"
if [[ ! -f $HARNESS || -L $HARNESS ]] || ! as_drill sudo -n true >/dev/null 2>&1 ||
  ! as_drill /usr/bin/docker info >/dev/null 2>&1; then
  preflight_failure restore_prerequisite_failed not_started
fi

if ! git -C "$REPO" show "$OPS_SHA:deploy/teagram/ops/restore_drill_inventory.py" > "$work/inventory.py" 2>/dev/null ||
  [[ ! -s $work/inventory.py ]]; then
  preflight_failure inventory_helper_unavailable not_started
fi
chmod 0600 "$work/inventory.py"

inventory_json="$work/dump-inventory.json"
collect_inventory() {
  /usr/bin/python3 "$work/inventory.py" "$DUMP_DIR" > "$inventory_json" 2>/dev/null
}
selection_fields() {
  /usr/bin/python3 - "$inventory_json" <<'PY'
import json
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as source:
        selected = json.load(source).get("selected")
except (OSError, ValueError, TypeError):
    raise SystemExit(1)
if selected is None:
    print("|".join(("", "", "", "", "no_deploy_dump", "")))
    raise SystemExit(0)
fields = (
    selected.get("name", ""),
    str(selected.get("size_bytes", "")),
    selected.get("size_class", ""),
    str(selected.get("age_seconds", "")),
    "ok" if selected.get("metadata_ok") is True else "refused",
    ",".join(selected.get("refusal_reasons", [])),
)
print("|".join(fields))
PY
}

teardown_runtime() {
  local path=$1
  [[ -n $path ]] || return 0
  if [[ ! -e $path && ! -L $path ]]; then return 0; fi
  if [[ ! $path =~ ^/tmp/teagram-restore-readiness\.[A-Za-z0-9]{8}$ ||
    -L $path || ! -d $path || $(realpath -e -- "$path" 2>/dev/null) != "$path" ||
    $(stat -c '%u:%a' -- "$path" 2>/dev/null) != "$drill_uid:700" ]]; then
    return 1
  fi
  as_drill /usr/bin/timeout --signal=TERM --kill-after=15s 5m /bin/bash "$HARNESS" \
    --teardown-prepared "$path" >/dev/null 2>&1 || return 1
  [[ ! -e $path && ! -L $path ]]
}

teardown_prepared() {
  [[ -n ${prepared_dir:-} ]] || return 0
  teardown_runtime "$prepared_dir"
}

teardown_new_prepared() {
  local new_paths
  new_paths=$(/usr/bin/python3 - <<'PY'
from pathlib import Path

paths = sorted(Path("/tmp").glob("teagram-restore-readiness.*"))
if len(paths) > 1:
    raise SystemExit(1)
if paths:
    print(paths[0])
PY
  ) || return 1
  [[ -n $new_paths ]] || return 0
  prepared_dir=$new_paths
  teardown_prepared
}

post_prepare_failure() {
  local category=$1 cleanup_status=verified
  teardown_prepared || cleanup_status=unverified
  printf '{"result":"fail","phase":"preflight","category":"%s","cleanup":"%s","exit":1}\n' \
    "$category" "$cleanup_status"
  exit 1
}
print_inventory_csv() {
  /usr/bin/python3 - "$inventory_json" <<'PY'
import csv
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    inventory = json.load(source)["inventory"]
writer = csv.writer(sys.stdout, lineterminator="\n")
writer.writerow(("filename", "size_bytes", "mtime_utc", "prune_if_keep_newest_10"))
for row in inventory:
    writer.writerow(
        (
            row["name"],
            row["size_bytes"],
            row["mtime_utc"],
            "yes" if row["prune_if_keep_newest_10"] else "no",
        )
    )
PY
}

if ! collect_inventory; then preflight_failure dump_inventory_unavailable not_started; fi
IFS='|' read -r dump_name dump_size dump_size_class dump_age dump_metadata dump_refusal < <(selection_fields) ||
  preflight_failure dump_inventory_invalid not_started
if [[ $dump_metadata != ok ]]; then
  failure_category=isolation_refusal
  [[ -n $dump_name ]] || failure_category=no_deploy_dump
  printf 'dump_metadata_refusal filename=%s reasons=%s\n' "${dump_name:-none}" "${dump_refusal:-no_deploy_dump}"
  printf '%s\n' 'dump_inventory_csv_begin'
  print_inventory_csv
  printf '%s\n' 'dump_inventory_csv_end'
  preflight_failure "$failure_category" not_started
fi
if ! [[ $dump_name =~ ^pgdump-pre-[0-9a-f]{7,40}-[0-9]{8}T[0-9]{6}Z\.sql$ ]]; then
  preflight_failure invalid_dump_name not_started
fi

existing_prepared=$(/usr/bin/python3 - <<'PY'
from pathlib import Path

print(sum(1 for path in Path("/tmp").glob("teagram-restore-readiness.*")))
PY
)
if [[ $existing_prepared != 0 ]]; then preflight_failure existing_prepared_runtime not_started; fi
existing_run_dirs=$(/usr/bin/python3 - <<'PY'
from pathlib import Path

print(sum(1 for path in Path("/run").glob("restore-readiness-*")))
PY
)
if [[ $existing_run_dirs != 0 ]]; then preflight_failure existing_run_directory not_started; fi

disk_before_prepare=$(/usr/bin/python3 -c 'import shutil; print(shutil.disk_usage("/root").free)' 2>/dev/null) ||
  preflight_failure headroom_unavailable not_started
if (( disk_before_prepare < PREPARE_MIN_DISK_BYTES )); then
  preflight_failure insufficient_disk_for_prepare not_started
fi

prep_output="$work/prepare.out"
prep_error="$work/prepare.err"
if ! as_drill timeout --signal=TERM --kill-after=15s 45m /bin/bash "$HARNESS" --prepare-only > "$prep_output" 2> "$prep_error"; then
  cleanup_status=verified
  teardown_new_prepared || cleanup_status=unverified
  printf '{"result":"fail","phase":"prepare","category":"preparation_failed","cleanup":"%s","exit":1}\n' "$cleanup_status"
  exit 1
fi
prepared_dir=$(/usr/bin/python3 - "$prep_output" <<'PY'
import json
import re
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as source:
        result = json.load(source)
except (OSError, ValueError):
    raise SystemExit(1)
path = result.get("prepared_dir") if isinstance(result, dict) else None
if (
    result.get("result") != "prepared"
    or not isinstance(path, str)
    or re.fullmatch(r"/tmp/teagram-restore-readiness\.[A-Za-z0-9]{8}", path) is None
):
    raise SystemExit(1)
print(path)
PY
) || {
  cleanup_status=verified
  teardown_new_prepared || cleanup_status=unverified
  printf '{"result":"fail","phase":"prepare","category":"invalid_prepared_runtime","cleanup":"%s","exit":1}\n' "$cleanup_status"
  exit 1
}
if [[ -L $prepared_dir || ! -d $prepared_dir || $(realpath -e -- "$prepared_dir" 2>/dev/null) != "$prepared_dir" ||
  $(stat -c '%u:%a' -- "$prepared_dir" 2>/dev/null) != "$drill_uid:700" ]]; then
  cleanup_status=verified
  teardown_prepared || cleanup_status=unverified
  printf '{"result":"fail","phase":"prepare","category":"invalid_prepared_runtime","cleanup":"%s","exit":1}\n' "$cleanup_status"
  exit 1
fi

if ! acquire_deploy_lock; then
  cleanup_status=verified
  teardown_prepared || cleanup_status=unverified
  printf '{"result":"fail","phase":"preflight","category":"rollout_in_progress","cleanup":"%s","exit":1}\n' "$cleanup_status"
  exit 1
fi

if ! collect_inventory; then
  cleanup_status=verified
  teardown_prepared || cleanup_status=unverified
  printf '{"result":"fail","phase":"preflight","category":"dump_inventory_unavailable","cleanup":"%s","exit":1}\n' "$cleanup_status"
  exit 1
fi
IFS='|' read -r dump_name dump_size dump_size_class dump_age dump_metadata dump_refusal < <(selection_fields) || {
  cleanup_status=verified
  teardown_prepared || cleanup_status=unverified
  printf '{"result":"fail","phase":"preflight","category":"dump_inventory_invalid","cleanup":"%s","exit":1}\n' "$cleanup_status"
  exit 1
}
if [[ $dump_metadata != ok || ! $dump_name =~ ^pgdump-pre-[0-9a-f]{7,40}-[0-9]{8}T[0-9]{6}Z\.sql$ ]]; then
  cleanup_status=verified
  teardown_prepared || cleanup_status=unverified
  failure_category=isolation_refusal
  [[ -n $dump_name ]] || failure_category=no_deploy_dump
  printf 'dump_metadata_refusal filename=%s reasons=%s\n' "${dump_name:-none}" "${dump_refusal:-no_deploy_dump}"
  printf '%s\n' 'dump_inventory_csv_begin'
  print_inventory_csv
  printf '%s\n' 'dump_inventory_csv_end'
  printf '{"result":"fail","phase":"preflight","category":"%s","cleanup":"%s","exit":1}\n' "$failure_category" "$cleanup_status"
  exit 1
fi

if ! /usr/bin/python3 - "$work/headroom.json" <<'PY'
import json
import os
import shutil
import time

def cpu_totals():
    with open("/proc/stat", encoding="ascii") as source:
        fields = source.readline().split()
    values = [int(value) for value in fields[1:]]
    return sum(values), values[3] + (values[4] if len(values) > 4 else 0)

first_total, first_idle = cpu_totals()
time.sleep(1)
last_total, last_idle = cpu_totals()
delta_total = last_total - first_total
delta_idle = last_idle - first_idle
idle_percent = round(100 * delta_idle / delta_total, 1) if delta_total > 0 else 0.0
memory_available = 0
with open("/proc/meminfo", encoding="ascii") as source:
    for line in source:
        if line.startswith("MemAvailable:"):
            memory_available = int(line.split()[1]) * 1024
            break
result = {
    "disk_available_bytes": shutil.disk_usage("/root").free,
    "memory_available_bytes": memory_available,
    "cpu_cores": os.cpu_count() or 0,
    "cpu_idle_percent": idle_percent,
}
with open(__import__("sys").argv[1], "w", encoding="ascii") as target:
    json.dump(result, target, separators=(",", ":"))
PY
then
  post_prepare_failure headroom_unavailable
fi
headroom_fields=$(/usr/bin/python3 - "$work/headroom.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="ascii") as source:
    result = json.load(source)
print("\t".join(str(result[key]) for key in ("disk_available_bytes", "memory_available_bytes", "cpu_cores", "cpu_idle_percent")))
PY
) || post_prepare_failure headroom_unavailable
IFS=$'\t' read -r disk_available memory_available cpu_cores cpu_idle <<<"$headroom_fields"
required_disk=$((2 * dump_size))
(( required_disk < PREPARE_MIN_DISK_BYTES )) && required_disk=$PREPARE_MIN_DISK_BYTES
headroom_ok=yes
(( disk_available >= required_disk )) || headroom_ok=no
(( memory_available >= RUN_MIN_MEMORY_BYTES )) || headroom_ok=no
awk -v value="$cpu_idle" -v minimum="$RUN_MIN_IDLE_PERCENT" 'BEGIN { exit !(value >= minimum) }' || headroom_ok=no
if [[ $headroom_ok != yes ]]; then
  printf 'headroom disk_available_bytes=%s memory_available_bytes=%s cpu_cores=%s cpu_idle_percent=%s\n' \
    "$disk_available" "$memory_available" "$cpu_cores" "$cpu_idle"
  post_prepare_failure insufficient_headroom
fi

identity_after_prepare=$(live_identity 2>/dev/null) || post_prepare_failure live_snapshot_failed
if [[ $identity_after_prepare != "$identity" ]]; then post_prepare_failure live_changed_during_prepare; fi
live_identity > "$work/live.before" 2>/dev/null || post_prepare_failure live_snapshot_failed
volume_snapshot() {
  local names sorted_names name created_at
  names=$(/usr/bin/docker volume ls --quiet) || return 1
  sorted_names=$(printf '%s\n' "$names" | sort) || return 1
  while IFS= read -r name; do
    [[ $name == teagram_* ]] || continue
    created_at=$(/usr/bin/docker volume inspect --format '{{.CreatedAt}}' "$name") || return 1
    printf '%s\t%s\n' "$name" "$created_at"
  done <<< "$sorted_names"
}
volume_snapshot > "$work/volumes.before" 2>/dev/null || post_prepare_failure volume_snapshot_failed
tailscale serve status --json > "$work/serve.before.json" 2>/dev/null || post_prepare_failure serve_snapshot_failed
/usr/bin/python3 -m json.tool "$work/serve.before.json" >/dev/null 2>&1 || post_prepare_failure serve_snapshot_invalid
admin_probe() {
  local result
  result=$(curl --silent --show-error --max-time 10 --output /dev/null \
    --write-out '%{http_code} %{ssl_verify_result}' "$READINESS_URL" 2>/dev/null) || return 1
  [[ $result == '401 0' ]]
}
admin_probe || post_prepare_failure live_readiness_failed

run_dirs_before="$work/run-dirs.before"
/usr/bin/python3 - "$run_dirs_before" <<'PY'
from pathlib import Path
import sys

with open(sys.argv[1], "w", encoding="ascii") as target:
    for path in sorted(Path("/run").glob("restore-readiness-*")):
        target.write(path.name + "\n")
PY

dump_path="$DUMP_DIR/$dump_name"
drill_output="$work/drill.out"
drill_error="$work/drill.err"
started_ns=$(/usr/bin/python3 -c 'import time; print(time.time_ns())')
drill_status=0
if as_drill timeout --signal=TERM --kill-after=15s 35m /bin/bash "$HARNESS" \
  --local-dump "$dump_path" --prepared-dir "$prepared_dir" > "$drill_output" 2> "$drill_error"; then
  drill_status=0
else
  drill_status=$?
fi
finished_ns=$(/usr/bin/python3 -c 'import time; print(time.time_ns())')
duration_ms=$(((finished_ns - started_ns) / 1000000))

report_json="$work/drill-report.json"
report_valid=no
if /usr/bin/python3 - "$drill_output" "$report_json" "$drill_status" <<'PY'
import json
import re
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as source:
        report = json.load(source)
except (OSError, ValueError):
    raise SystemExit(1)
if not isinstance(report, dict):
    raise SystemExit(1)

phases = {
    "application_assertions",
    "auth_key_clearing",
    "cleanup",
    "daemon_startup",
    "fixture_generation",
    "pending_migration",
    "preflight",
    "restore",
    "schema_validation",
    "source_migrations",
}
categories = {
    "assertion_failure",
    "assertions_passed",
    "auth_failure",
    "auth_key_clear_failure",
    "cleanup_failure",
    "fault_test_failure",
    "fixture_failure",
    "identity_collision",
    "interrupted",
    "isolation_refusal",
    "migration_failure",
    "preflight_failure",
    "provisioning_failure",
    "restore_failure",
    "rollout_in_progress",
    "schema_failure",
    "startup_failure",
    "timeout",
}

def valid_common(result: dict[str, object]) -> bool:
    return (
        result.get("migration_head") == "not_reached"
        or re.fullmatch(r"[0-9]{14}", str(result.get("migration_head", ""))) is not None
    ) and result.get("help_getConfig") in {"pass", "fail", "not_run"} and result.get(
        "synthetic_signin"
    ) in {"pass", "fail", "not_run"} and type(result.get("elapsed_ms")) is int and result[
        "elapsed_ms"
    ] >= 0

if report.get("result") == "pass":
    expected = {
        "result",
        "migration_head",
        "help_getConfig",
        "synthetic_signin",
        "auth_keys_cleared",
        "cleanup",
        "elapsed_ms",
        "exit",
        "input_mode",
    }
    if (
        set(report) != expected
        or sys.argv[3] != "0"
        or not valid_common(report)
        or re.fullmatch(r"[0-9]{14}", str(report.get("migration_head", ""))) is None
        or report.get("help_getConfig") != "pass"
        or report.get("synthetic_signin") != "pass"
        or report.get("auth_keys_cleared") != "pass"
        or report.get("cleanup") != "verified"
        or report.get("exit") != 0
        or report.get("input_mode") != "local"
    ):
        raise SystemExit(1)
    safe_report = {key: report[key] for key in expected}
else:
    expected = {
        "result",
        "phase",
        "category",
        "migration_head",
        "help_getConfig",
        "synthetic_signin",
        "cleanup",
        "elapsed_ms",
        "exit",
    }
    if (
        set(report) != expected
        or sys.argv[3] == "0"
        or report.get("result") != "fail"
        or report.get("phase") not in phases
        or report.get("category") not in categories
        or not valid_common(report)
        or report.get("cleanup") not in {"verified", "unverified", "not_started"}
        or report.get("exit") != 1
    ):
        raise SystemExit(1)
    safe_report = {key: report[key] for key in expected}
with open(sys.argv[2], "w", encoding="utf-8") as target:
    json.dump(safe_report, target, ensure_ascii=True, separators=(",", ":"))
PY
then
  report_valid=yes
fi
if [[ $report_valid != yes ]]; then
  failure_phase=preflight
  failure_category=invalid_drill_output
  if [[ $drill_status == 124 || $drill_status == 137 || $drill_status == 143 ]]; then
    failure_phase=timeout
    failure_category=outer_timeout
  fi
  /usr/bin/python3 - "$report_json" "$failure_phase" "$failure_category" <<'PY'
import json
import sys

report = {
    "result": "fail",
    "phase": sys.argv[2],
    "category": sys.argv[3],
    "cleanup": "unknown",
    "exit": 1,
}
with open(sys.argv[1], "w", encoding="utf-8") as target:
    json.dump(report, target, separators=(",", ":"))
PY
fi

drill_fields=$(/usr/bin/python3 - "$report_json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    result = json.load(source)
keys = (
    "result",
    "help_getConfig",
    "synthetic_signin",
    "auth_keys_cleared",
    "input_mode",
    "cleanup",
    "elapsed_ms",
    "exit",
)
print("|".join(str(result.get(key, "")) for key in keys))
PY
)
IFS='|' read -r drill_result drill_help_get_config drill_synthetic_signin drill_auth_keys_cleared \
  drill_input_mode drill_cleanup drill_elapsed_ms drill_exit <<<"$drill_fields"

cp -- "$inventory_json" "$work/dump-inventory.before.json"

recovery_result=not_needed
if [[ $drill_cleanup != verified && $drill_cleanup != not_started ]]; then
  run_dirs_after="$work/run-dirs.after"
  /usr/bin/python3 - "$run_dirs_after" <<'PY'
from pathlib import Path
import sys

with open(sys.argv[1], "w", encoding="ascii") as target:
    for path in sorted(Path("/run").glob("restore-readiness-*")):
        target.write(path.name + "\n")
PY
  manifest=$(/usr/bin/python3 - "$run_dirs_before" "$run_dirs_after" <<'PY'
from pathlib import Path
import sys

before = set(Path(sys.argv[1]).read_text(encoding="ascii").splitlines())
after = set(Path(sys.argv[2]).read_text(encoding="ascii").splitlines())
created = sorted(after - before)
if len(created) == 1:
    print("/run/" + created[0] + "/ownership.json")
PY
)
  if [[ -n $manifest && $manifest =~ ^/run/restore-readiness-[A-Za-z0-9._-]+/ownership\.json$ &&
    ! -L $manifest && -f $manifest && $(stat -c '%u:%a' -- "$manifest" 2>/dev/null) == 0:600 ]]; then
    recovery_status=0
    as_drill timeout --signal=TERM --kill-after=15s 5m /bin/bash "$HARNESS" \
      --recover "$manifest" > "$work/recover.out" 2> "$work/recover.err" || recovery_status=$?
    run_dir=${manifest%/ownership.json}
    if [[ ! -e $run_dir && ! -L $run_dir ]]; then
      recovery_result="verified_exit_${recovery_status}"
      teardown_prepared || recovery_result=verified_run_cleanup_only
    else
      recovery_result=unverified
    fi
  else
    recovery_result=manifest_unavailable
  fi
fi

post_status=pass
live_status=pass
volume_status=pass
serve_status=pass
readiness_status=pass
prepared_status=absent
inventory_status=pass
if ! collect_inventory; then
  inventory_status=failed
else
  cp -- "$inventory_json" "$work/dump-inventory.after.json"
  if ! /usr/bin/python3 - "$work/dump-inventory.before.json" "$work/dump-inventory.after.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as before_file:
    before = json.load(before_file).get("selected")
with open(sys.argv[2], encoding="utf-8") as after_file:
    after = json.load(after_file).get("selected")
keys = ("name", "size_bytes", "mtime_utc", "metadata_ok")
if before is None or after is None or any(before.get(key) != after.get(key) for key in keys):
    raise SystemExit(1)
PY
  then
    inventory_status=failed
  fi
fi
live_identity > "$work/live.after" 2>/dev/null || live_status=failed
if [[ $live_status == pass ]] && ! cmp -s "$work/live.before" "$work/live.after"; then live_status=failed; fi
volume_snapshot > "$work/volumes.after" 2>/dev/null || volume_status=failed
if [[ $volume_status == pass ]] && ! cmp -s "$work/volumes.before" "$work/volumes.after"; then volume_status=failed; fi
tailscale serve status --json > "$work/serve.after.json" 2>/dev/null || serve_status=failed
if [[ $serve_status == pass ]] && ! cmp -s "$work/serve.before.json" "$work/serve.after.json"; then serve_status=failed; fi
admin_probe || readiness_status=failed
if [[ -n ${prepared_dir:-} && ( -e $prepared_dir || -L $prepared_dir ) ]]; then
  if [[ $drill_cleanup == verified || $recovery_result == verified_* || $drill_cleanup == not_started ]]; then
    teardown_prepared || prepared_status=unverified
  else
    prepared_status=unverified
  fi
fi
[[ $live_status == pass && $volume_status == pass && $serve_status == pass &&
  $readiness_status == pass && $prepared_status == absent && $inventory_status == pass ]] || post_status=failed

release_deploy_lock || exit 1
final_status=0
if [[ $post_status != pass || $drill_result != pass || $drill_cleanup != verified || $drill_exit != 0 || $drill_status != 0 ||
  $drill_input_mode != local || $drill_auth_keys_cleared != pass ||
  $drill_help_get_config != pass || $drill_synthetic_signin != pass ]]; then
  final_status=1
fi
printf -v summary_output 'base_sha=%s main_ci=green_via_deploy\ndump_filename=%s dump_size_bytes=%s dump_size_class=%s dump_age_seconds=%s\nwall_clock_duration_ms=%s harness_elapsed_ms=%s headroom_disk_available_bytes=%s headroom_memory_available_bytes=%s cpu_cores=%s cpu_idle_percent=%s\nlive_teagramd=%s teagram_volumes=%s serve_config=%s readiness=%s prepared_runtime=%s dump_metadata=%s post_run=%s recovery=%s' \
  "$live_sha" "$dump_name" "$dump_size" "$dump_size_class" "$dump_age" \
  "$duration_ms" "$drill_elapsed_ms" "$disk_available" "$memory_available" "$cpu_cores" "$cpu_idle" \
  "$live_status" "$volume_status" "$serve_status" "$readiness_status" "$prepared_status" "$inventory_status" "$post_status" "$recovery_result"
report_output=$(/usr/bin/python3 - "$report_json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    report = json.load(source)
print(json.dumps(report, ensure_ascii=True, separators=(",", ":")))
PY
)
inventory_json="$work/dump-inventory.before.json"
inventory_output=$(print_inventory_csv)
publish_final_output "$summary_output" "$report_output" "$inventory_output" "$final_status"
