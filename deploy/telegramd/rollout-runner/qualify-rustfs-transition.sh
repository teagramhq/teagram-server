#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)

if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'RustFS qualification requires uid 0 for root-owned evidence checks' >&2
  exit 77
fi

if [ "$#" -ne 3 ] || [ "$1" != check ]; then
  printf '%s\n' 'usage: qualify-rustfs-transition.sh check PRIVATE_BUNDLE_DIR CANDIDATE_CHECKOUT' >&2
  exit 64
fi

exec python3 "$SCRIPT_DIR/qualify-rustfs-transition.py" "$@"
