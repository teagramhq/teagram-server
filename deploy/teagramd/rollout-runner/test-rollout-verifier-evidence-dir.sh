#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$(id -u)" != 0 ]]; then
  echo 'test requires root to check the verifier evidence ownership gate' >&2
  exit 1
fi

script_dir="$(cd "$(dirname "$0")" && pwd)"
export ROLLOUT_VERIFIER_SOURCE_ONLY=1
source "$script_dir/rollout-verifier.sh"

evidence=$(mktemp -d /tmp/teagram-replay-check.test.XXXXXX)
link="${evidence}.link"
cleanup() {
  case "$evidence" in
    /tmp/teagram-replay-check.test.*) rm -rf -- "$evidence" ;;
    *) echo 'refusing to clean an unguarded evidence test path' >&2; return 1 ;;
  esac
  case "$link" in
    /tmp/teagram-replay-check.test.*) rm -f -- "$link" ;;
    *) echo 'refusing to clean an unguarded evidence test link' >&2; return 1 ;;
  esac
}
trap cleanup EXIT

secure_evidence_dir "$evidence"
if secure_evidence_dir /tmp/teagram-unapproved-evidence >/dev/null 2>&1; then
  echo 'FAIL verifier accepted an unapproved /tmp evidence path' >&2
  exit 1
fi
if secure_evidence_dir "$evidence/nested" >/dev/null 2>&1; then
  echo 'FAIL verifier accepted a nested replay evidence path' >&2
  exit 1
fi
ln -s "$evidence" "$link"
if secure_evidence_dir "$link" >/dev/null 2>&1; then
  echo 'FAIL verifier accepted a symlinked replay evidence path' >&2
  exit 1
fi

printf '%s\n' 'rollout verifier replay evidence path cases pass'
