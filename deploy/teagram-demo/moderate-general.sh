#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)

usage() {
  cat <<'EOF'
Usage:
  moderate-general.sh delete-message MESSAGE_ID
  moderate-general.sh ban-user USERNAME
EOF
}

if (($# != 2)); then
  usage >&2
  exit 2
fi

case "$1" in
  delete-message)
    [[ "$2" =~ ^[1-9][0-9]*$ ]] || { usage >&2; echo 'message ID must be a positive integer' >&2; exit 2; }
    exec "$SCRIPT_DIR/seed-demo.sh" --moderate-delete-message --message-id "$2"
    ;;
  ban-user)
    [[ "$2" =~ ^[A-Za-z0-9_]{5,32}$ ]] || { usage >&2; echo 'username must be 5 to 32 letters, digits, or underscores without @' >&2; exit 2; }
    exec "$SCRIPT_DIR/seed-demo.sh" --moderate-ban-user --username "$2"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
