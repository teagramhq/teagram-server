#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WORK_DIR=$(mktemp -d /tmp/teagram-demo-scheduling-test.XXXXXXXX)
HOLDER_PID=''
cleanup() {
  local status=$?
  trap - EXIT HUP INT TERM
  if [[ -n "$HOLDER_PID" ]]; then
    kill "$HOLDER_PID" 2>/dev/null || true
    wait "$HOLDER_PID" 2>/dev/null || true
  fi
  case "$WORK_DIR" in
    /tmp/teagram-demo-scheduling-test.????????) rm -rf -- "$WORK_DIR" || status=1 ;;
    *) printf '%s\n' 'test refused to remove an unexpected temporary path' >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

fail() {
  printf 'demo scheduling test: %s\n' "$1" >&2
  exit 1
}

# Start a background holder of the lock, then wait until it has the lock.
start_holder() {
  local mode=$1 lock=$2 ready=$3
  rm -f -- "$ready"
  (
    exec 7<"$lock"
    flock "$mode" 7
    : >"$ready"
    exec sleep 60
  ) &
  HOLDER_PID=$!
  local tries=0
  until [[ -e "$ready" ]]; do
    tries=$((tries + 1))
    ((tries < 200)) || fail 'lock holder did not start'
    sleep 0.05
  done
}

stop_holder() {
  kill "$HOLDER_PID" 2>/dev/null || true
  wait "$HOLDER_PID" 2>/dev/null || true
  HOLDER_PID=''
}

# Cron helpers: the crontab and pgrep are mocked, so the test needs no daemon.
BIN="$WORK_DIR/bin"
CRON_STORE="$WORK_DIR/crontab.txt"
mkdir -p "$BIN"
cat > "$BIN/crontab" <<'SH'
#!/bin/sh
set -eu
case "${1:-}" in
  -l)
    [ -f "$CRON_STORE" ] || { printf '%s\n' 'no crontab for root' >&2; exit 1; }
    cat "$CRON_STORE"
    ;;
  -) cat > "$CRON_STORE" ;;
  *) printf 'unexpected crontab call: %s\n' "$*" >&2; exit 1 ;;
esac
SH
cat > "$BIN/pgrep" <<'SH'
#!/bin/sh
[ "${MOCK_CROND:-0}" = 1 ]
SH
chmod 0755 "$BIN/crontab" "$BIN/pgrep"
export CRON_STORE
export PATH="$BIN:$PATH"

# shellcheck source=deploy/teagram-demo/demo-cron.sh
source "$SCRIPT_DIR/demo-cron.sh"
# shellcheck source=deploy/teagram-demo/activity-guards.sh
source "$SCRIPT_DIR/activity-guards.sh"

# Missing daemon: the installer must refuse and name crond.
if output=$(MOCK_CROND=0 demo_cron_require_daemon 2>&1); then
  fail 'daemon check accepted a host without a running cron daemon'
fi
[[ "$output" == *crond* ]] || fail "daemon message does not name crond: $output"
MOCK_CROND=1 demo_cron_require_daemon || fail 'daemon check rejected a running cron daemon'

# Cron entries: an unrelated job survives, and each install is idempotent.
printf '%s\n' '17 4 * * * /usr/bin/true # unrelated job' > "$CRON_STORE"
for _ in 1 2; do
  demo_cron_install_entry activity '*/10 * * * *' \
    /opt/teagram-demo/deploy/teagram-demo/run-activity.sh /var/log/teagram-demo/activity.log
  demo_cron_install_entry weekly-wipe '0 3 * * 0' \
    /opt/teagram-demo/deploy/teagram-demo/wipe-weekly.sh /var/log/teagram-demo/weekly-wipe.log
done
[[ "$(grep -Fc -- '# teagram-demo:activity' "$CRON_STORE")" == 1 ]] || fail 'activity entry is not exactly one line'
[[ "$(grep -Fc -- '# teagram-demo:weekly-wipe' "$CRON_STORE")" == 1 ]] || fail 'weekly wipe entry is not exactly one line'
[[ "$(grep -Fc -- 'unrelated job' "$CRON_STORE")" == 1 ]] || fail 'unrelated crontab line was not preserved'
grep -Eq '^\*/10 \* \* \* \* cd /opt/teagram-demo && env PATH=[^ ]+ /opt/teagram-demo/deploy/teagram-demo/run-activity.sh >>/var/log/teagram-demo/activity.log 2>&1 # teagram-demo:activity$' "$CRON_STORE" ||
  fail 'activity cron line does not match the expected schedule and command'
grep -Eq '^0 3 \* \* 0 cd /opt/teagram-demo && env PATH=[^ ]+ /opt/teagram-demo/deploy/teagram-demo/wipe-weekly.sh >>/var/log/teagram-demo/weekly-wipe.log 2>&1 # teagram-demo:weekly-wipe$' "$CRON_STORE" ||
  fail 'weekly wipe cron line does not match the expected schedule and command'

# Activity inventory: must be a regular, non-symlink, mode 0600 file owned by the expected uid.
OWNER=$(id -u)
INVENTORY="$WORK_DIR/inventory.json"
printf '{}\n' > "$INVENTORY"
chmod 0600 "$INVENTORY"
demo_activity_inventory_check "$INVENTORY" "$OWNER" || fail 'valid inventory was rejected'
demo_activity_inventory_check "$INVENTORY" 0 && fail 'inventory owned by another uid was accepted'
chmod 0644 "$INVENTORY"
demo_activity_inventory_check "$INVENTORY" "$OWNER" && fail 'inventory with mode 0644 was accepted'
chmod 0600 "$INVENTORY"
ln -s "$INVENTORY" "$WORK_DIR/inventory-link.json"
demo_activity_inventory_check "$WORK_DIR/inventory-link.json" "$OWNER" && fail 'symlinked inventory was accepted'
demo_activity_inventory_check "$WORK_DIR/missing.json" "$OWNER" && fail 'missing inventory was accepted'

# Wipe lock: created safely, refused when unsafe.
LOCK="$WORK_DIR/activity-state/wipe.lock"
mkdir -p "$WORK_DIR/activity-state"
demo_wipe_lock_prepare "$LOCK" "$OWNER" || fail 'wipe lock could not be created'
[[ "$(stat -c '%a' "$LOCK")" == 600 ]] || fail 'wipe lock is not mode 0600'
ln -s "$WORK_DIR/elsewhere" "$WORK_DIR/activity-state/link.lock"
demo_wipe_lock_prepare "$WORK_DIR/activity-state/link.lock" "$OWNER" && fail 'dangling symlink lock was accepted'
[[ ! -e "$WORK_DIR/elsewhere" ]] || fail 'lock preparation followed a symlink'
demo_wipe_lock_prepare "$LOCK" 0 && fail 'wipe lock owned by another uid was accepted'

# Lock behaviour with real flock: activity skips while the wipe holds the lock,
# the wipe waits only a bounded time for activity, and both proceed when free.
READY="$WORK_DIR/holder.ready"
(demo_activity_try_shared_lock "$LOCK") || fail 'activity could not take the free lock'

start_holder -x "$LOCK" "$READY"
if (demo_activity_try_shared_lock "$LOCK"); then
  fail 'activity ran while the wipe held the lock'
fi
if (demo_wipe_take_exclusive_lock "$LOCK" 1); then
  fail 'wipe acquired a lock already held exclusively'
fi
stop_holder

start_holder -s "$LOCK" "$READY"
(demo_activity_try_shared_lock "$LOCK") || fail 'activity was refused while another activity held the lock shared'
if (demo_wipe_take_exclusive_lock "$LOCK" 1); then
  fail 'wipe proceeded while scheduled activity was still running'
fi
stop_holder

(demo_wipe_take_exclusive_lock "$LOCK" 5) || fail 'wipe could not take the free lock after activity stopped'

printf '%s\n' 'demo scheduling holds: cron entries idempotent, activity and wipe share the lock, inventory is checked'
