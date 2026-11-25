#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"
# shellcheck source=deploy/teagram-demo/activity-guards.sh
source "$SCRIPT_DIR/activity-guards.sh"

ACTIVITY_SETTINGS=/etc/default/teagram-demo-activity
ACTIVITY_STATE_DIR=/var/lib/teagram-demo-activity
ACTIVITY_INVENTORY=/var/lib/teagram-demo/secrets/demo-accounts/inventory.json

demo_run_activity() {
  local state_dir=${1:-$ACTIVITY_STATE_DIR}
  local inventory=${2:-$ACTIVITY_INVENTORY}
  local owner=${3:-0}
  local settings=${4:-$ACTIVITY_SETTINGS}
  local wipe_lock="$state_dir/wipe.lock" lock_status

  demo_require_root || return $?

  if [[ -e "$settings" ]]; then
    [[ -f "$settings" && ! -L "$settings" && "$(stat -c '%u:%a' "$settings")" == "$owner:600" ]] || {
      demo_fail 'activity settings must be a regular root-owned mode 0600 file'
      return 1
    }
    set -a
    # shellcheck source=/dev/null
    source "$settings"
    set +a
  fi

  demo_activity_state_check "$state_dir" "$owner" || {
    demo_fail 'activity state directory must be a safe mode 0700 directory'
    return 1
  }
  demo_activity_inventory_check "$inventory" "$owner" || {
    demo_fail 'activity inventory must be a root-owned mode 0600 regular file'
    return 1
  }

  demo_require_env || return $?

  demo_wipe_lock_prepare "$wipe_lock" "$owner" || {
    demo_fail 'wipe lock is missing or unsafe'
    return 1
  }
  if demo_activity_try_shared_lock "$wipe_lock"; then
    :
  else
    lock_status=$?
    if [[ "$lock_status" == 1 ]]; then
      printf '%s\n' 'demo activity: skipped: wipe running'
      return 0
    fi
    return "$lock_status"
  fi

  TARGET="${TEAGRAM_DEMO_ACTIVITY_TARGET:-river}"
  MAX_PER_HOUR="${TEAGRAM_DEMO_ACTIVITY_MAX_PER_HOUR:-6}"
  MIN_INTERVAL="${TEAGRAM_DEMO_ACTIVITY_MIN_INTERVAL:-5m}"
  demo_compose run --rm --no-deps --pull never \
    --volume "$inventory:/run/secrets/demo-activity-inventory.json:ro" \
    demo-activity \
    --activity \
    --target "$TARGET" \
    --max-actions-per-hour "$MAX_PER_HOUR" \
    --min-interval "$MIN_INTERVAL" || return $?
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  demo_run_activity
fi
