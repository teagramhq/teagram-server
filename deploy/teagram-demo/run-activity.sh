#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"
# shellcheck source=deploy/teagram-demo/activity-credential.sh
source "$SCRIPT_DIR/activity-credential.sh"

demo_require_root
demo_require_env

[[ -d /var/lib/teagram-demo-activity && ! -L /var/lib/teagram-demo-activity ]] ||
  demo_fail 'activity state directory is missing or unsafe'
[[ "$(stat -c '%u:%a' /var/lib/teagram-demo-activity)" == '0:700' ]] ||
  demo_fail 'activity state directory must be root-owned mode 0700'

ACTIVITY_CREDENTIAL_FILE=$(demo_stage_activity_credential)
trap 'demo_cleanup_activity_credential "$ACTIVITY_CREDENTIAL_FILE"' EXIT

TARGET="${TEAGRAM_DEMO_ACTIVITY_TARGET:-river}"
MAX_PER_HOUR="${TEAGRAM_DEMO_ACTIVITY_MAX_PER_HOUR:-6}"
MIN_INTERVAL="${TEAGRAM_DEMO_ACTIVITY_MIN_INTERVAL:-5m}"
demo_compose run --rm --no-deps --pull never \
  --volume "$ACTIVITY_CREDENTIAL_FILE:/run/secrets/demo-activity-inventory.json:ro" \
  demo-activity \
  --activity \
  --target "$TARGET" \
  --max-actions-per-hour "$MAX_PER_HOUR" \
  --min-interval "$MIN_INTERVAL"
