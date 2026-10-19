#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)

[[ "$EUID" -eq 0 ]] || { echo 'install-weekly-wipe.sh must run as root' >&2; exit 1; }
[[ "$REPO_ROOT" == /opt/teagram-demo ]] || {
  echo 'weekly wipe units are installed only from /opt/teagram-demo' >&2
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
"$SCRIPT_DIR/seed-demo.sh" --check

install -o root -g root -m 0644 "$SCRIPT_DIR/teagram-demo-weekly-wipe.service" \
  /etc/systemd/system/teagram-demo-weekly-wipe.service
install -o root -g root -m 0644 "$SCRIPT_DIR/teagram-demo-weekly-wipe.timer" \
  /etc/systemd/system/teagram-demo-weekly-wipe.timer
systemctl daemon-reload
systemctl enable --now teagram-demo-weekly-wipe.timer
systemctl list-timers teagram-demo-weekly-wipe.timer --no-pager
