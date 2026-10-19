#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
ACTIVITY_ENV_FILE=/etc/default/teagram-demo-activity
ACCOUNT_DIR=/var/lib/teagram-demo/secrets/demo-accounts
ACCOUNT_INVENTORY="$ACCOUNT_DIR/inventory.json"

[[ "$EUID" -eq 0 ]] || { echo 'install-activity.sh must run as root' >&2; exit 1; }
[[ "$REPO_ROOT" == /opt/teagram-demo ]] || {
  echo 'activity units are installed only from /opt/teagram-demo' >&2
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
[[ -f "$ACCOUNT_INVENTORY" && ! -L "$ACCOUNT_INVENTORY" && "$(stat -c '%u:%a' "$ACCOUNT_INVENTORY")" == '0:600' ]] || {
  echo 'the root-owned mode 0600 seeded account inventory is required' >&2
  exit 1
}
jq -e 'has("demo_tavi") and (.["demo_tavi"] | test("^[0-9a-f]{64}$"))' \
  "$ACCOUNT_INVENTORY" >/dev/null 2>&1 || {
  echo 'the seeded activity account credential is missing or invalid' >&2
  exit 1
}
docker image inspect teagramd:local >/dev/null 2>&1 || {
  echo 'the teagramd:local image must be built before enabling activity' >&2
  exit 1
}

if [[ ! -e "$ACTIVITY_ENV_FILE" ]]; then
  install -o root -g root -m 0600 "$SCRIPT_DIR/activity.env.example" "$ACTIVITY_ENV_FILE"
else
  [[ -f "$ACTIVITY_ENV_FILE" && ! -L "$ACTIVITY_ENV_FILE" && "$(stat -c '%u:%a' "$ACTIVITY_ENV_FILE")" == '0:600' ]] || {
    echo 'activity settings must be a regular root-owned mode 0600 file' >&2
    exit 1
  }
fi

install -o root -g root -m 0644 "$SCRIPT_DIR/teagram-demo-activity.service" \
  /etc/systemd/system/teagram-demo-activity.service
install -o root -g root -m 0644 "$SCRIPT_DIR/teagram-demo-activity.timer" \
  /etc/systemd/system/teagram-demo-activity.timer
systemctl daemon-reload
systemctl enable --now teagram-demo-activity.timer
systemctl list-timers teagram-demo-activity.timer --no-pager
