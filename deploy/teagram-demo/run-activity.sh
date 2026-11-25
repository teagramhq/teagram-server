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

demo_run_activity() (
  local state_dir=${1:-$ACTIVITY_STATE_DIR}
  local inventory=${2:-$ACTIVITY_INVENTORY}
  local activity_inventory=$inventory
  local owner=${3:-0}
  local settings=${4:-$ACTIVITY_SETTINGS}
  local wipe_lock="$state_dir/wipe.lock" legacy_lock="$state_dir/activity.lock"
  local container_lock="$state_dir/container-activity.lock" lock_status
  local image_ref image_lock_protocol
  local compatibility_dir=''
  local -a extra_volumes=()

  # cleanup runs from the EXIT trap, which ShellCheck cannot follow.
  # shellcheck disable=SC2329
  demo_activity_cleanup_compat() {
    local status=$?
    trap - EXIT
    if [[ -n "$compatibility_dir" ]] && ! rm -rf -- "$compatibility_dir"; then
      demo_fail 'could not remove the temporary legacy activity credential copy'
      status=1
    fi
    exit "$status"
  }
  trap demo_activity_cleanup_compat EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM

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
  demo_wipe_lock_prepare "$legacy_lock" "$owner" || {
    demo_fail 'legacy activity lock is missing or unsafe'
    return 1
  }
  if demo_activity_try_shared_lock "$wipe_lock" 9; then
    :
  else
    lock_status=$?
    if [[ "$lock_status" == 1 ]]; then
      printf '%s\n' 'demo activity: skipped: wipe running'
      return 0
    fi
    return "$lock_status"
  fi
  if demo_activity_try_shared_lock "$legacy_lock" 10; then
    :
  else
    lock_status=$?
    if [[ "$lock_status" == 1 ]]; then
      exec 9>&-
      exec 10>&-
      printf '%s\n' 'demo activity: skipped: wipe running'
      return 0
    fi
    exec 9>&-
    exec 10>&-
    return "$lock_status"
  fi

  if ! image_ref=$(demo_compose_image_ref); then
    exec 9>&-
    exec 10>&-
    return 1
  fi
  if ! image_lock_protocol=$(docker image inspect \
    --format '{{index .Config.Labels "com.teagram.demo.activity-lock"}}' "$image_ref"); then
    demo_fail 'could not inspect the activity image lock protocol'
    exec 9>&-
    exec 10>&-
    return 1
  fi
  case "$image_lock_protocol" in
    container-activity.lock)
      # The current image uses a separate in-container lock, so the host may
      # keep the shared legacy lock for the full activity run.
      ;;
    ''|'<no value>')
      # Older images require a 0400 inventory and use activity.lock inside
      # the container. Copy the credential privately and alias that old lock
      # path to the current container lock, keeping the host wipe lock held.
      compatibility_dir=$(mktemp -d /run/teagram-demo-activity.XXXXXXXX) || {
        demo_fail 'could not create a private temporary directory for legacy activity credentials'
        return 1
      }
      [[ -d "$compatibility_dir" && ! -L "$compatibility_dir" &&
        "$(stat -c '%u:%a' -- "$compatibility_dir")" == "$owner:700" ]] || {
        demo_fail 'legacy activity credential directory must be a private owner-owned mode 0700 directory'
        return 1
      }
      if ! install -o "$owner" -m 0400 -- "$inventory" "$compatibility_dir/inventory.json"; then
        demo_fail 'could not prepare the legacy activity credential copy'
        return 1
      fi
      [[ "$(stat -c '%u:%a' -- "$compatibility_dir/inventory.json")" == "$owner:400" ]] || {
        demo_fail 'legacy activity credential copy must be owner-owned mode 0400'
        return 1
      }
      activity_inventory="$compatibility_dir/inventory.json"
      demo_wipe_lock_prepare "$container_lock" "$owner" || {
        demo_fail 'container activity lock is missing or unsafe'
        return 1
      }
      extra_volumes+=(--volume "$container_lock:/var/lib/teagram-demo-activity/activity.lock:rw")
      ;;
    *)
      demo_fail "unsupported activity image lock protocol: $image_lock_protocol"
      exec 9>&-
      exec 10>&-
      return 1
      ;;
  esac
  export TEAGRAM_DEMO_IMAGE_REF="$image_ref"

  TARGET="${TEAGRAM_DEMO_ACTIVITY_TARGET:-river}"
  MAX_PER_HOUR="${TEAGRAM_DEMO_ACTIVITY_MAX_PER_HOUR:-6}"
  MIN_INTERVAL="${TEAGRAM_DEMO_ACTIVITY_MIN_INTERVAL:-5m}"
  demo_compose run --rm --no-deps --pull never \
    --volume "$activity_inventory:/run/secrets/demo-activity-inventory.json:ro" \
    "${extra_volumes[@]}" \
    demo-activity \
    --activity \
    --target "$TARGET" \
    --max-actions-per-hour "$MAX_PER_HOUR" \
    --min-interval "$MIN_INTERVAL" || return $?
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  demo_run_activity
fi
