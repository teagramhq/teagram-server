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
CRON_WRITES="$WORK_DIR/crontab-writes.txt"
touch "$CRON_WRITES"
mkdir -p "$BIN"
cat > "$BIN/crontab" <<'SH'
#!/bin/sh
set -eu
case "${1:-}" in
  -l)
    if [ -n "${CRONTAB_READ_ERROR:-}" ]; then
      printf '%s\n' "$CRONTAB_READ_ERROR" >&2
      exit 2
    fi
    [ -f "$CRON_STORE" ] || {
      if [ "${CRONTAB_ABSENT_STYLE:-gnu}" = busybox ]; then
        printf '%s\n' "crontab: can't open 'root': No such file or directory" >&2
      else
        printf '%s\n' 'no crontab for root' >&2
      fi
      exit 1
    }
    cat "$CRON_STORE"
    ;;
  -)
    printf '%s\n' write >> "$CRON_WRITES"
    cat > "$CRON_STORE"
    ;;
  *) printf 'unexpected crontab call: %s\n' "$*" >&2; exit 1 ;;
esac
SH
cat > "$BIN/pgrep" <<'SH'
#!/bin/sh
[ "${MOCK_CROND:-0}" = 1 ]
SH
chmod 0755 "$BIN/crontab" "$BIN/pgrep"
export CRON_STORE CRON_WRITES
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
CRONTAB_ABSENT_STYLE=busybox demo_cron_install_entry activity '*/10 * * * *' \
  /opt/teagram-demo/deploy/teagram-demo/run-activity.sh /var/log/teagram-demo/activity.log
[[ "$(grep -Fc -- '# teagram-demo:activity' "$CRON_STORE")" == 1 ]] || fail 'missing crontab was not initialized with the activity entry'

printf '%s\n' '17 4 * * * /usr/bin/true # unrelated job' > "$CRON_STORE"
before_store=$(cat "$CRON_STORE")
before_writes=$(wc -l < "$CRON_WRITES")
if output=$(CRONTAB_READ_ERROR='crontab: permission denied' demo_cron_install_entry activity '*/10 * * * *' \
  /opt/teagram-demo/deploy/teagram-demo/run-activity.sh /var/log/teagram-demo/activity.log 2>&1); then
  fail 'cron installer accepted a real crontab read error'
fi
[[ "$output" == *'could not read the root crontab'* ]] || fail "read error was not reported: $output"
[[ "$(cat "$CRON_STORE")" == "$before_store" ]] || fail 'read error replaced the existing crontab'
[[ "$(wc -l < "$CRON_WRITES")" == "$before_writes" ]] || fail 'read error attempted to write the crontab'

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

# Existing systemd installs already have the activity container's lock. The
# first updated activity run must create the separate host coordination lock.
MIGRATION_DIR="$WORK_DIR/legacy-state"
mkdir -m 0700 "$MIGRATION_DIR"
LEGACY_LOCK="$MIGRATION_DIR/activity.lock"
MIGRATED_LOCK="$MIGRATION_DIR/wipe.lock"
: > "$LEGACY_LOCK"
chmod 0600 "$LEGACY_LOCK"
demo_wipe_lock_prepare "$MIGRATED_LOCK" "$OWNER" || fail 'updated runner did not create the host coordination lock'
[[ "$(stat -c '%i' "$LEGACY_LOCK")" != "$(stat -c '%i' "$MIGRATED_LOCK")" ]] ||
  fail 'host wipe lock was coupled to the container activity lock'
start_holder -x "$LEGACY_LOCK" "$WORK_DIR/legacy-holder.ready"
(demo_activity_try_shared_lock "$MIGRATED_LOCK") || fail 'activity host lock interfered with the container activity lock'
stop_holder

# A preexisting systemd wipe service cannot write the new name. It must still
# use the existing activity lock until the activity service creates the new lock.
FALLBACK_DIR="$WORK_DIR/legacy-fallback"
mkdir -m 0700 "$FALLBACK_DIR"
FALLBACK_LOCK="$FALLBACK_DIR/activity.lock"
: > "$FALLBACK_LOCK"
chmod 0600 "$FALLBACK_LOCK"
demo_wipe_lock_create() { return 1; }
if selected_lock=$(demo_wipe_lock_select "$FALLBACK_DIR/wipe.lock" "$OWNER"); then
  :
else
  fail 'existing systemd wipe could not fall back to the legacy activity lock'
fi
unset -f demo_wipe_lock_create
[[ "$selected_lock" == "$FALLBACK_LOCK" ]] || fail 'legacy wipe did not select the existing activity lock'
start_holder -x "$FALLBACK_LOCK" "$WORK_DIR/fallback-holder.ready"
if (demo_wipe_take_exclusive_lock "$selected_lock" 1); then
  fail 'legacy wipe ignored an in-flight activity using the old lock'
fi
stop_holder

printf '%s\n' 'demo scheduling holds: cron entries idempotent, activity and wipe share the lock, inventory is checked'
