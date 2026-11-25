#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
# shellcheck source=deploy/teagram-demo/demo-cron.sh
source "$SCRIPT_DIR/demo-cron.sh"
WIPE_LOG_DIR=/var/log/teagram-demo

[[ "$EUID" -eq 0 ]] || { echo 'install-weekly-wipe.sh must run as root' >&2; exit 1; }
[[ "$REPO_ROOT" == /opt/teagram-demo ]] || {
  echo 'weekly wipe cron entry is installed only from /opt/teagram-demo' >&2
  exit 1
}
git -C "$REPO_ROOT" ls-files --error-unmatch deploy/teagram-demo/seed-demo.sh >/dev/null 2>&1 || {
  echo 'weekly wipe is not installable until the tracked API seed-demo.sh is supplied' >&2
  exit 1
}
[[ -x "$SCRIPT_DIR/seed-demo.sh" ]] || {
  echo 'tracked API seed-demo.sh must be executable' >&2
  exit 1
}
demo_cron_require_daemon
demo_cron_require_utc
"$SCRIPT_DIR/seed-demo.sh" --check

mkdir -p "$WIPE_LOG_DIR"
chown root:root "$WIPE_LOG_DIR"
chmod 0750 "$WIPE_LOG_DIR"
demo_cron_install_entry weekly-wipe '0 3 * * 0' \
  "$REPO_ROOT/deploy/teagram-demo/wipe-weekly.sh" "$WIPE_LOG_DIR/weekly-wipe.log"
printf '%s\n' 'weekly wipe scheduled in root crontab: 0 3 * * 0 (UTC)'
