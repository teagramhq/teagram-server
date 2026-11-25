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
WIPE_LOCK="$ACTIVITY_STATE_DIR/wipe.lock"

demo_require_root

if [[ -e "$ACTIVITY_SETTINGS" ]]; then
  [[ -f "$ACTIVITY_SETTINGS" && ! -L "$ACTIVITY_SETTINGS" && "$(stat -c '%u:%a' "$ACTIVITY_SETTINGS")" == '0:600' ]] ||
    demo_fail 'activity settings must be a regular root-owned mode 0600 file'
  set -a
  # shellcheck source=/dev/null
  source "$ACTIVITY_SETTINGS"
  set +a
fi

[[ -d "$ACTIVITY_STATE_DIR" && ! -L "$ACTIVITY_STATE_DIR" ]] ||
  demo_fail 'activity state directory is missing or unsafe'
[[ "$(stat -c '%u:%a' "$ACTIVITY_STATE_DIR")" == '0:700' ]] ||
  demo_fail 'activity state directory must be root-owned mode 0700'

demo_activity_inventory_check "$ACTIVITY_INVENTORY" 0 ||
  demo_fail 'activity inventory must be a root-owned mode 0600 regular file'

demo_require_env

demo_wipe_lock_prepare "$WIPE_LOCK" 0 || demo_fail 'wipe lock is missing or unsafe'
if ! demo_activity_try_shared_lock "$WIPE_LOCK"; then
  printf '%s\n' 'demo activity: skipped: wipe running'
  exit 0
fi

TARGET="${TEAGRAM_DEMO_ACTIVITY_TARGET:-river}"
MAX_PER_HOUR="${TEAGRAM_DEMO_ACTIVITY_MAX_PER_HOUR:-6}"
MIN_INTERVAL="${TEAGRAM_DEMO_ACTIVITY_MIN_INTERVAL:-5m}"
demo_compose run --rm --no-deps --pull never \
  --volume "$ACTIVITY_INVENTORY:/run/secrets/demo-activity-inventory.json:ro" \
  demo-activity \
  --activity \
  --target "$TARGET" \
  --max-actions-per-hour "$MAX_PER_HOUR" \
  --min-interval "$MIN_INTERVAL"
