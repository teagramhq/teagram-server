#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'run RustFS qualification fixtures as root' >&2
  exit 77
fi

exec python3 "$SCRIPT_DIR/test-qualify-rustfs-transition.py"
