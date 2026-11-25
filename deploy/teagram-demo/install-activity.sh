#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"
# shellcheck source=deploy/teagram-demo/demo-cron.sh
source "$SCRIPT_DIR/demo-cron.sh"
# shellcheck source=deploy/teagram-demo/activity-guards.sh
source "$SCRIPT_DIR/activity-guards.sh"
ACTIVITY_ENV_FILE=/etc/default/teagram-demo-activity
ACTIVITY_STATE_DIR=/var/lib/teagram-demo-activity
ACCOUNT_DIR=/var/lib/teagram-demo/secrets/demo-accounts
ACCOUNT_INVENTORY="$ACCOUNT_DIR/inventory.json"
ACTIVITY_LOG_DIR=/var/log/teagram-demo

[[ "$EUID" -eq 0 ]] || { echo 'install-activity.sh must run as root' >&2; exit 1; }
[[ "$REPO_ROOT" == /opt/teagram-demo ]] || {
  echo 'activity cron entries are installed only from /opt/teagram-demo' >&2
  exit 1
}
git -C "$REPO_ROOT" ls-files --error-unmatch deploy/teagram-demo/seed-demo.sh >/dev/null 2>&1 || {
  echo 'scheduled activity requires the tracked API seed-demo.sh' >&2
  exit 1
}
[[ -x "$SCRIPT_DIR/seed-demo.sh" && -x "$SCRIPT_DIR/run-activity.sh" ]] || {
  echo 'tracked seed and activity scripts must be executable' >&2
  exit 1
}
demo_activity_inventory_check "$ACCOUNT_INVENTORY" 0 || {
  echo 'the root-owned mode 0600 seeded account inventory is required' >&2
  exit 1
}
jq -e 'has("demo_ren") and (.["demo_ren"] | test("^[0-9a-f]{64}$"))' \
  "$ACCOUNT_INVENTORY" >/dev/null 2>&1 || {
  echo 'the seeded activity account credential is missing or invalid' >&2
  exit 1
}
IMAGE_REF=$(demo_active_image_ref)
docker image inspect "$IMAGE_REF" >/dev/null 2>&1 || {
  echo 'the active teagramd image must be present before enabling activity' >&2
  exit 1
}
demo_cron_require_daemon
demo_cron_require_utc

if [[ ! -e "$ACTIVITY_ENV_FILE" ]]; then
  install -o root -g root -m 0600 "$SCRIPT_DIR/activity.env.example" "$ACTIVITY_ENV_FILE"
else
  [[ -f "$ACTIVITY_ENV_FILE" && ! -L "$ACTIVITY_ENV_FILE" && "$(stat -c '%u:%a' "$ACTIVITY_ENV_FILE")" == '0:600' ]] || {
    echo 'activity settings must be a regular root-owned mode 0600 file' >&2
    exit 1
  }
fi

mkdir -p "$ACTIVITY_STATE_DIR" "$ACTIVITY_LOG_DIR"
chown root:root "$ACTIVITY_STATE_DIR"
chmod 0700 "$ACTIVITY_STATE_DIR"
chmod 0750 "$ACTIVITY_LOG_DIR"
WIPE_LOCK="$ACTIVITY_STATE_DIR/wipe.lock"
demo_wipe_lock_prepare "$WIPE_LOCK" 0 || {
  echo 'the shared wipe lock is missing or unsafe' >&2
  exit 1
}

demo_cron_install_entry activity '*/10 * * * *' \
  "$REPO_ROOT/deploy/teagram-demo/run-activity.sh" "$ACTIVITY_LOG_DIR/activity.log"
printf '%s\n' 'activity scheduled in root crontab: */10 * * * *'
