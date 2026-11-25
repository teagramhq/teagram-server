#!/bin/sh
set -eu
umask 077

REPO=/opt/teagram-demo
WIPE_REL=deploy/teagram-demo/wipe-weekly.sh
WIPE="$REPO/$WIPE_REL"
OPS_SHA=${OPS_SHA:-}
WIPE_LOG_DIR=/var/log/teagram-demo
WIPE_LOG="$WIPE_LOG_DIR/weekly-wipe.log"

fail() {
  printf 'teagram-demo weekly wipe: %s\n' "$1" >&2
  exit 1
}

case "$OPS_SHA" in
  ''|*[!0123456789abcdef]*) fail 'reviewed SHA is invalid' ;;
esac
[ "${#OPS_SHA}" -eq 40 ] || fail 'reviewed SHA must be 40 hexadecimal characters'
[ -d "$REPO/.git" ] && [ ! -L "$REPO" ] || fail 'demo checkout is unavailable'
[ -d "$REPO/.state" ] && [ ! -L "$REPO/.state" ] || fail 'demo state path is unavailable or unsafe'

LOCK_PATH=$REPO/.state/deploy.lock
[ ! -L "$LOCK_PATH" ] || fail 'demo deployment lock must not be a symlink'
if ! exec 9>>"$LOCK_PATH"; then
  fail 'could not open the demo deployment lock'
fi
flock -x 9 || fail 'could not acquire the demo deployment lock'

head=$(git -C "$REPO" rev-parse HEAD) || fail 'could not read the demo checkout revision'
[ "$head" = "$OPS_SHA" ] || fail 'demo checkout differs from the reviewed SHA'
status=$(git --no-optional-locks -C "$REPO" status --porcelain --untracked-files=all) ||
  fail 'could not inspect the demo checkout'
[ -z "$status" ] || fail 'demo checkout has tracked or untracked changes'
[ -f "$WIPE" ] && [ ! -L "$WIPE" ] && [ -x "$WIPE" ] ||
  fail 'wipe script is missing or not executable'
expected=$(git -C "$REPO" rev-parse "$OPS_SHA:$WIPE_REL") || fail 'reviewed wipe script is missing'
actual=$(git -C "$REPO" hash-object "$WIPE") || fail 'could not read the live wipe script'
[ "$actual" = "$expected" ] || fail 'live wipe script differs from the reviewed SHA'

if [ -L "$WIPE_LOG_DIR" ] || [ -L "$WIPE_LOG" ]; then
  fail 'wipe log path is a symlink'
fi
if ! mkdir -p "$WIPE_LOG_DIR"; then
  fail 'could not prepare the wipe log directory'
fi

print_wipe_log_tail() {
  if [ -f "$WIPE_LOG" ]; then
    printf '%s\n' 'last weekly wipe log lines:' >&2
    tail -n 40 "$WIPE_LOG" >&2 || printf '%s\n' 'could not read the weekly wipe log tail' >&2
  else
    printf '%s\n' 'weekly wipe log is not available' >&2
  fi
}

status=0
output=$(DEMO_DEPLOY_LOCK_HELD=1 OPS_SHA="$OPS_SHA" "$WIPE" 2>>"$WIPE_LOG") || status=$?
[ -z "$output" ] || printf '%s\n' "$output"
if [ "$status" -ne 0 ]; then
  printf 'teagram-demo weekly wipe failed with exit status %s\n' "$status" >&2
  print_wipe_log_tail
  exit "$status"
fi

verification=$(printf '%s\n' "$output" | awk '/^weekly wipe=verified / { line = $0 } END { print line }')
case "$verification" in
  'weekly wipe=verified '*) ;;
  *)
    printf '%s\n' 'teagram-demo weekly wipe completed without its verification line' >&2
    print_wipe_log_tail
    exit 1
    ;;
esac
