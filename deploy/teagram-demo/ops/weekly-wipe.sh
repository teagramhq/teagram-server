#!/usr/bin/env bash
set -Eeuo pipefail

SERVICE=teagram-demo-weekly-wipe.service
STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)

systemctl reset-failed "$SERVICE"
systemctl start "$SERVICE"
[[ "$(systemctl show --property=Result --value "$SERVICE")" == success ]] || {
  printf 'teagram-demo weekly wipe did not finish successfully\n' >&2
  exit 1
}

VERIFICATION=$(journalctl --unit "$SERVICE" --since "$STARTED_AT" --output cat --no-pager |
  awk '/^weekly wipe=verified / { line = $0 } END { print line }')
[[ "$VERIFICATION" == weekly\ wipe=verified\ * ]] || {
  printf 'teagram-demo weekly wipe completed without its verification line\n' >&2
  exit 1
}
printf '%s\n' "$VERIFICATION"
