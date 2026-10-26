#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RUNNER="$SCRIPT_DIR/rollout-runner.sh"
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"

approved_verifier_sha=$(sed -n 's/^[[:space:]]*readonly APPROVED_VERIFIER_SHA=//p' "$RUNNER")
actual_verifier_sha=$(sha256sum "$VERIFIER" | awk '{print $1}')

if [ "$approved_verifier_sha" = "$actual_verifier_sha" ]; then
  printf '%s\n' 'PASS rollout runner approves the current verifier artifact'
else
  printf 'FAIL rollout verifier pin: approved=%s actual=%s\n' \
    "$approved_verifier_sha" "$actual_verifier_sha" >&2
  exit 1
fi
