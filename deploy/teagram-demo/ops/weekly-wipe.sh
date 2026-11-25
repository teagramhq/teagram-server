#!/bin/sh
set -eu

WIPE=/opt/teagram-demo/deploy/teagram-demo/wipe-weekly.sh
if [ ! -f "$WIPE" ] || [ -L "$WIPE" ] || [ ! -x "$WIPE" ]; then
  printf 'teagram-demo weekly wipe: %s\n' 'wipe script is missing or not executable' >&2
  exit 1
fi

status=0
output=$("$WIPE") || status=$?
[ -z "$output" ] || printf '%s\n' "$output"
if [ "$status" -ne 0 ]; then
  printf 'teagram-demo weekly wipe failed with exit status %s\n' "$status" >&2
  exit "$status"
fi

verification=$(printf '%s\n' "$output" | awk '/^weekly wipe=verified / { line = $0 } END { print line }')
case "$verification" in
  'weekly wipe=verified '*) ;;
  *)
    printf '%s\n' 'teagram-demo weekly wipe completed without its verification line' >&2
    exit 1
    ;;
esac
