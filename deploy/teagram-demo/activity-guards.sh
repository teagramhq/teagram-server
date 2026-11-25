#!/usr/bin/env bash
# Guards shared by scheduled activity, the weekly wipe, and their tests. Sourced only.
#
# The wipe holds the wipe lock and legacy activity lock exclusively for the
# whole reset. Activity takes both shared and non-blocking so old systemd wipes
# keep coordinating during the lock migration. The container uses its own
# container-activity.lock to prevent overlapping activity posts.

demo_activity_inventory_check() {
  local inventory=$1 owner=$2
  [[ -f "$inventory" && ! -L "$inventory" && "$(stat -c '%u:%a' -- "$inventory")" == "$owner:600" ]]
}

demo_activity_state_check() {
  local state_dir=$1 owner=$2
  [[ -d "$state_dir" && ! -L "$state_dir" && "$(stat -c '%u:%a' -- "$state_dir")" == "$owner:700" ]]
}

demo_wipe_lock_check() {
  local lock=$1 owner=$2
  [[ -f "$lock" && ! -L "$lock" && "$(stat -c '%u:%a' -- "$lock")" == "$owner:600" ]]
}

demo_wipe_lock_create() {
  local lock=$1 error
  if error=$( (umask 077 && set -o noclobber && : >"$lock") 2>&1); then
    return 0
  fi
  # A concurrent creator may have installed the path; prepare() still checks
  # its type, owner, and mode before accepting it.
  if [[ -e "$lock" || -L "$lock" ]]; then
    return 0
  fi
  printf 'teagram-demo: could not create shared wipe lock %s: %s\n' "$lock" "${error:-creation failed}" >&2
  return 1
}

demo_wipe_lock_prepare() {
  local lock=$1 owner=$2
  if [[ ! -e "$lock" && ! -L "$lock" ]]; then
    demo_wipe_lock_create "$lock" || return 1
  fi
  demo_wipe_lock_check "$lock" "$owner"
}

demo_wipe_lock_state_dir_writable() {
  local state_dir=$1
  [[ -d "$state_dir" && ! -L "$state_dir" && -w "$state_dir" ]]
}

# The old wipe unit ran under ProtectSystem=strict without a writable activity
# state path. Only that sandboxed case may use the old activity lock.
demo_wipe_lock_select() {
  local lock=$1 owner=$2 legacy="${1%/*}/activity.lock"
  if [[ -e "$lock" || -L "$lock" ]]; then
    demo_wipe_lock_check "$lock" "$owner" || return 1
    printf '%s\n' "$lock"
    return 0
  fi
  if ! demo_wipe_lock_state_dir_writable "${lock%/*}"; then
    if ! demo_wipe_lock_check "$legacy" "$owner"; then
      printf '%s\n' 'teagram-demo: activity state directory is not writable and the legacy activity lock is missing or unsafe' >&2
      return 1
    fi
    printf '%s\n' 'teagram-demo: activity state directory is not writable; using the legacy activity lock' >&2
    printf '%s\n' "$legacy"
    return 0
  fi
  if ! demo_wipe_lock_prepare "$lock" "$owner"; then
    printf 'teagram-demo: could not create or validate shared wipe lock %s\n' "$lock" >&2
    return 1
  fi
  printf '%s\n' "$lock"
}

# Returns 1 only when a wipe holds the lock. The descriptor stays open for the
# rest of the caller's run, which keeps the shared lock until it exits.
demo_activity_try_shared_lock() {
  local lock=$1 fd=${2:-9} flock_output flock_status
  command -v flock >/dev/null 2>&1 || {
    demo_fail 'flock is not installed; cannot check the activity lock'
    return 2
  }
  case "$fd" in
    9)
      if ! exec 9<"$lock"; then
        demo_fail "could not open the shared activity lock $lock"
        return 2
      fi
      ;;
    10)
      if ! exec 10<"$lock"; then
        demo_fail "could not open the shared activity lock $lock"
        return 2
      fi
      ;;
    *)
      demo_fail "unsupported activity lock descriptor $fd"
      return 2
      ;;
  esac
  if flock_output=$(flock -s -n "$fd" 2>&1); then
    return 0
  else
    flock_status=$?
  fi
  if [[ "$flock_status" == 1 && -z "$flock_output" ]]; then
    return 1
  fi
  demo_fail "flock failed while checking the activity lock (exit $flock_status): ${flock_output:-no diagnostic output}"
  return 2
}

# Returns non-zero when an activity run still holds the lock after the timeout.
# The selected descriptor stays open until the wipe exits, so no activity
# starts while the reset runs.
demo_wipe_take_exclusive_lock() {
  local lock=$1 timeout=$2 fd=${3:-8}
  case "$fd" in
    7) exec 7<"$lock" || return 1 ;;
    8) exec 8<"$lock" || return 1 ;;
    *) demo_fail "unsupported wipe lock descriptor $fd"; return 1 ;;
  esac
  flock -x -w "$timeout" "$fd"
}
