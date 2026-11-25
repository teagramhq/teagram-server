#!/bin/sh
set -eu
umask 077

WIPE=/opt/teagram-demo/deploy/teagram-demo/wipe-weekly.sh
WIPE_LOG_DIR=/var/log/teagram-demo
WIPE_LOG="$WIPE_LOG_DIR/weekly-wipe.log"
if [ ! -f "$WIPE" ] || [ -L "$WIPE" ] || [ ! -x "$WIPE" ]; then
  printf 'teagram-demo weekly wipe: %s\n' 'wipe script is missing or not executable' >&2
  exit 1
fi
if [ -L "$WIPE_LOG_DIR" ] || [ -L "$WIPE_LOG" ]; then
  printf 'teagram-demo weekly wipe: %s\n' 'wipe log path is a symlink' >&2
  exit 1
fi
if ! mkdir -p "$WIPE_LOG_DIR"; then
  printf 'teagram-demo weekly wipe: %s\n' 'could not prepare the wipe log directory' >&2
  exit 1
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
output=$("$WIPE" 2>>"$WIPE_LOG") || status=$?
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
