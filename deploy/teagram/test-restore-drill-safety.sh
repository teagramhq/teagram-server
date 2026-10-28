#!/usr/bin/env bash
set -euo pipefail
umask 077

script="$(cd "$(dirname "$0")" && pwd)/ops/restore-drill.sh"
tmp=$(mktemp -d)
trap '/bin/rm -rf -- "$tmp"' EXIT
functions="$tmp/restore-drill-functions.sh"
sed -n \
  -e '/^checkout_is_clean() {/,/^}/p' \
  -e '/^remove_work_directory() {/,/^}/p' \
  -e '/^publish_final_output() {/,/^}/p' \
  "$script" > "$functions"
[[ -s $functions ]]
# shellcheck disable=SC1090
source "$functions"

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
