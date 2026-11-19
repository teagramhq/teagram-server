#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

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
    demo_valid_signup_username "$2" || { usage >&2; echo 'username must follow signup rules and must not include @' >&2; exit 2; }
    exec "$SCRIPT_DIR/seed-demo.sh" --moderate-ban-user --username "$2"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
