#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname -- "$0")" && pwd)
exec python3 "$SCRIPT_DIR/schema-result-gate.py" "$@"
