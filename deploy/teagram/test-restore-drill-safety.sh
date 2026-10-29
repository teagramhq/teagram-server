#!/usr/bin/env bash
set -euo pipefail
umask 077

script="$(cd "$(dirname "$0")" && pwd)/ops/restore-drill.sh"
tmp=$(mktemp -d)
trap '/bin/rm -rf -- "$tmp"' EXIT
functions="$tmp/restore-drill-functions.sh"
sed -n \
  -e '/^acquire_operation_lock() {/,/^}/p' \
  -e '/^checkout_is_clean() {/,/^}/p' \
  -e '/^remove_work_directory() {/,/^}/p' \
  -e '/^publish_final_output() {/,/^}/p' \
  -e '/^teardown_new_prepared() {/,/^}/p' \
  -e '/^volume_snapshot() {/,/^}/p' \
  "$script" | sed "s|/usr/bin/docker|$tmp/bin/docker|g" > "$functions"
[[ -s $functions ]]
mkdir -p "$tmp/bin" "$tmp/failing-sort"
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == volume && $2 == ls && $3 == --quiet ]]; then
  printf '%s\n' teagram_tgkey teagram_pgdata
elif [[ $1 == volume && $2 == inspect && $3 == --format && $4 == '{{.CreatedAt}}' ]]; then
  case $5 in
    teagram_pgdata) printf '%s\n' '2026-10-20 12:00:00 +0000 UTC' ;;
    teagram_tgkey) printf '%s\n' '2026-10-21 12:00:00 +0000 UTC' ;;
    *) exit 1 ;;
  esac
else
  exit 99
fi
DOCKER
cat > "$tmp/failing-sort/sort" <<'SORT'
#!/bin/sh
exit 42
SORT
chmod 755 "$tmp/bin/docker" "$tmp/failing-sort/sort"
# shellcheck disable=SC1090
source "$functions"

operation_lock_fd=
OPERATION_LOCK="$tmp/restore-drill.operation.lock"
if ! acquire_operation_lock; then
  printf '%s\n' 'FAIL could not acquire operation lock' >&2
  exit 1
fi
operation_runtime=$(mktemp -d /tmp/teagram-restore-readiness.XXXXXXXX)
printf '%s\n' 'owned by first invocation' > "$operation_runtime/sentinel"
competing_status=0
if (
  exec {operation_lock_fd}>&-
  OPERATION_LOCK="$OPERATION_LOCK" bash -c '
    source "$1"
    teardown_prepared() {
      /bin/rm -rf -- "$prepared_dir"
    }
    acquire_operation_lock || exit $?
    teardown_new_prepared
  ' _ "$functions"
); then
  competing_status=0
else
  competing_status=$?
fi
[[ $competing_status == 1 && -f $operation_runtime/sentinel ]]
/bin/rm -rf -- "$operation_runtime"
exec {operation_lock_fd}>&-
lock_line=$(grep -nF 'if acquire_operation_lock; then' "$script" | cut -d: -f1)
runtime_line=$(grep -nF "existing_prepared=\$(/usr/bin/python3" "$script" | cut -d: -f1)
[[ -n $lock_line && -n $runtime_line && $lock_line -lt $runtime_line ]]

snapshot=$(volume_snapshot)
[[ $snapshot == $'teagram_pgdata\t2026-10-20 12:00:00 +0000 UTC\nteagram_tgkey\t2026-10-21 12:00:00 +0000 UTC' ]]
snapshot_status=0
if snapshot=$(PATH="$tmp/failing-sort:$PATH" volume_snapshot); then
  snapshot_status=0
else
  snapshot_status=$?
fi
[[ $snapshot_status == 1 && -z $snapshot ]]

guard_calls=$(grep -Fc "if checkout_is_clean \"\$CHECKOUT\"; then" "$script")
[[ $guard_calls == 2 ]]
status_commands=$(grep -Fc 'status --porcelain --untracked-files=all --ignored' "$script")
[[ $status_commands == 1 ]]

as_drill() {
  case "$MOCK_CHECKOUT_RESULT" in
    clean) return 0 ;;
    dirty) printf '%s\n' ' M tracked-file'; return 0 ;;
    error) printf '%s\n' 'partial status'; return 23 ;;
    *) return 99 ;;
  esac
}

MOCK_CHECKOUT_RESULT=clean
checkout_status=0
checkout_is_clean "$tmp" || checkout_status=$?
[[ $checkout_status == 0 ]]
MOCK_CHECKOUT_RESULT=dirty
checkout_status=0
checkout_is_clean "$tmp" || checkout_status=$?
[[ $checkout_status == 1 ]]
MOCK_CHECKOUT_RESULT=error
checkout_status=0
checkout_is_clean "$tmp" || checkout_status=$?
[[ $checkout_status == 2 ]]

stat() {
  if [[ ${1:-} == -c && ${2:-} == %u:%a ]]; then
    builtin printf '%s\n' '0:700'
  else
    command stat "$@"
  fi
}

rm() {
  case "${MOCK_RM_MODE:-real}" in
    fail) return 70 ;;
    noop) return 0 ;;
    real) command rm "$@" ;;
    *) return 99 ;;
  esac
}

readonly cleanup_failure='{"result":"fail","phase":"cleanup","category":"host_work_cleanup_failed","cleanup":"unverified","exit":1}'
for rm_mode in fail noop; do
  failure_work=$(mktemp -d /tmp/teagram-restore-drill.XXXXXXXX)
  work=$failure_work
  MOCK_RM_MODE=$rm_mode
  ASSERT_CLEAN_BEFORE_PRINT=0
  status=0
  if output=$(publish_final_output 'private-summary' '{"result":"pass","cleanup":"verified","exit":0}' 'private-inventory' 0); then
    printf 'FAIL cleanup mode %s published a success result\n' "$rm_mode" >&2
    exit 1
  else
    status=$?
  fi
  [[ $status == 1 && $output == "$cleanup_failure" && -d $failure_work ]]
  [[ $output != *private-* && $output != *'"result":"pass"'* ]]
  /bin/rm -rf -- "$failure_work"
done

success_work=$(mktemp -d /tmp/teagram-restore-drill.XXXXXXXX)
# The extracted publisher reads this global work path.
# shellcheck disable=SC2034
work=$success_work
MOCK_RM_MODE=real
ASSERT_CLEAN_BEFORE_PRINT=1
ASSERT_WORK_PATH=$success_work
printf() {
  if [[ ${ASSERT_CLEAN_BEFORE_PRINT:-0} == 1 &&
    ( -e ${ASSERT_WORK_PATH:-} || -L ${ASSERT_WORK_PATH:-} ) ]]; then
    return 97
  fi
  builtin printf '%s\n' "${2:-}"
}
output=$(publish_final_output 'summary' '{"result":"pass","cleanup":"verified","exit":0}' 'inventory' 0)
[[ ! -e $success_work && ! -L $success_work ]]
[[ $output == $'summary\n{"result":"pass","cleanup":"verified","exit":0}\ndump_inventory_csv_begin\ninventory\ndump_inventory_csv_end' ]]

printf '%s\n' 'restore drill safety fixtures pass'
