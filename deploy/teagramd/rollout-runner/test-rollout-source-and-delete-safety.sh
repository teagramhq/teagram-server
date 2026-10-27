#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHECKOUT=$(git -C "$SCRIPT_DIR/../../.." rev-parse --show-toplevel)
TMP=$(mktemp -d /tmp/main2612-rollout-safety.XXXXXXXX)
OWNED="$TMP/owned"
TOKEN=$(source "$SCRIPT_DIR/rollout-safe-remove-tree.sh"; rollout_create_owned_dir "$OWNED")
CHECKOUT_FIXTURE="$OWNED/checkout"
OUTSIDE="$TMP/outside"
mkdir -m 700 "$CHECKOUT_FIXTURE" "$OUTSIDE"

cleanup() {
  local marker="$OWNED/.rollout-owned-$TOKEN"
  if [ -d "$OWNED/remove-me" ]; then
    rollout_safe_remove_tree "$OWNED/remove-me" "$OWNED" "$TOKEN" "$CHECKOUT" || return 1
  fi
  rmdir "$OUTSIDE" "$CHECKOUT_FIXTURE"
  rm -f -- "$marker"
  rmdir "$OWNED" "$TMP"
}

caller_exit_trap() {
  cleanup
}

EVIDENCE_ROOT="$TMP/caller-evidence"
ROOT="$TMP/caller-root"
TARGET_DIR="$TMP/caller-target"
BASELINE_DIR="$TMP/caller-baseline"
trap caller_exit_trap EXIT
caller_trap_before=$(trap -p EXIT)
options_before=$(set +o)
umask_before=$(umask)

ROLLOUT_RUNNER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-runner.sh"
ROLLOUT_VERIFIER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-verifier.sh"

[ "$EVIDENCE_ROOT" = "$TMP/caller-evidence" ]
[ "$ROOT" = "$TMP/caller-root" ]
[ "$TARGET_DIR" = "$TMP/caller-target" ]
[ "$BASELINE_DIR" = "$TMP/caller-baseline" ]
[ "$(trap -p EXIT)" = "$caller_trap_before" ]
[ "$(set +o)" = "$options_before" ]
[ "$(umask)" = "$umask_before" ]
printf '%s\n' 'PASS sourcing runner and verifier preserves caller variables, trap, options, and umask'

if ! (initialize_runner_config && initialize_runner_config) >"$TMP/reinitialize.out" 2>&1; then
  printf '%s\n' 'FAIL runner configuration cannot be initialized twice' >&2
  cat "$TMP/reinitialize.out" >&2
  rm -f -- "$TMP/reinitialize.out"
  exit 1
fi
rm -f -- "$TMP/reinitialize.out"
printf '%s\n' 'PASS runner configuration initializes again after a fixture initialized it'

for protected in / /root /opt /opt/teagram "$CHECKOUT"; do
  if rollout_safe_remove_tree "$protected" "$OWNED" "$TOKEN" "$CHECKOUT"; then
    printf 'FAIL recursive removal accepted protected path: %s\n' "$protected" >&2
    exit 1
  fi
done
printf '%s\n' 'PASS recursive removal rejects root, deployment paths, and checkout'

if rollout_safe_remove_tree "$OUTSIDE" "$OWNED" "$TOKEN" "$CHECKOUT"; then
  printf '%s\n' 'FAIL recursive removal accepted a path outside its owned directory' >&2
  exit 1
fi
if rollout_safe_remove_tree "$OWNED" "$OWNED" "$TOKEN" "$CHECKOUT"; then
  printf '%s\n' 'FAIL recursive removal accepted its owned directory itself' >&2
  exit 1
fi
printf '%s\n' 'PASS recursive removal requires a strict child of a run-owned directory'

mkdir -m 700 "$OWNED/remove-me"
mkdir -m 700 "$OWNED/remove-me/nested"
: > "$OWNED/remove-me/nested/fixture"
rollout_safe_remove_tree "$OWNED/remove-me" "$OWNED" "$TOKEN" "$CHECKOUT"
[ ! -e "$OWNED/remove-me" ]
printf '%s\n' 'PASS recursive removal deletes a run-owned child'
