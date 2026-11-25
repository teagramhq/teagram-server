#!/usr/bin/env bash
# Guards shared by scheduled activity, the weekly wipe, and their tests. Sourced only.
#
# The wipe holds the wipe lock exclusively for the whole reset. Each activity
# run holds it shared and non-blocking, so a wipe in progress makes activity
# skip, while the container's own activity.lock still refuses overlapping
# activity posts.

demo_activity_inventory_check() {
  local inventory=$1 owner=$2
  [[ -f "$inventory" && ! -L "$inventory" && "$(stat -c '%u:%a' -- "$inventory")" == "$owner:600" ]]
}

demo_wipe_lock_prepare() {
  local lock=$1 owner=$2
  if [[ ! -e "$lock" && ! -L "$lock" ]]; then
    (umask 077 && : >"$lock") || return 1
  fi
  [[ -f "$lock" && ! -L "$lock" && "$(stat -c '%u:%a' -- "$lock")" == "$owner:600" ]]
}

# Returns non-zero when a wipe holds the lock. The descriptor stays open on fd 9
# for the rest of the caller's run, which keeps the shared lock until it exits.
demo_activity_try_shared_lock() {
  local lock=$1
  exec 9<"$lock" || return 1
  flock -s -n 9
}

# Returns non-zero when an activity run still holds the lock after the timeout.
# The descriptor stays open on fd 8 until the wipe exits, so no activity starts
# while the reset runs.
demo_wipe_take_exclusive_lock() {
  local lock=$1 timeout=$2
  exec 8<"$lock" || return 1
  flock -x -w "$timeout" 8
}
